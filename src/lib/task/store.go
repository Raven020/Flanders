package task

// store.go is the task *store and selector* (spec 01 §select, spec 02 §deps): the
// step-1 "select" of the iteration anatomy. Where task.go models one file, the
// Store is the whole plan in memory — it enumerates specs/tasks/*.md, resolves the
// dependency graph, and answers the only question the loop asks each iteration:
// "what is the next actionable task?"
//
// Two design points carry the weight here:
//
//   - Id normalization. task.go stores `id` and `deps` *verbatim* so zero-padding
//     round-trips losslessly (`0007`, `[0001, 0005]`). That means the store, not
//     the model, owns turning "0001" and "1" into the same key when it matches a
//     dep to the task it names. normID does exactly that, and it is the only place
//     ids are compared — so a dep written `01` still resolves to task `1`.
//   - Cycles are an error, never a silent stall. A dependency cycle makes every
//     task in it permanently ineligible; if Next just returned "nothing" the loop
//     would look done when it is actually wedged. So Next runs full-graph cycle
//     detection first and surfaces a *CycleError naming the loop, which the plan
//     loop can act on (spec 02: the harness owns dependency ordering).

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Store is an in-memory view of the plan: every task under specs/tasks/, indexed
// by normalized id and held in a stable selection order (ascending id). It is a
// read-time snapshot — reload after the on-disk files change.
type Store struct {
	tasks []*Task          // sorted by id (see lessID); the selection order
	byID  map[string]*Task // keyed by normID(id) — the dep→task resolver
}

// CycleError reports a dependency cycle. Cycle lists the normalized ids on the
// loop in traversal order with the entry id repeated at the end (a→b→a), so the
// message reads as the path that closes back on itself.
type CycleError struct {
	Cycle []string
}

func (e *CycleError) Error() string {
	return "task: dependency cycle: " + strings.Join(e.Cycle, " -> ")
}

// UnknownDepError reports a task whose dep names no task in the store — a
// malformed plan the plan loop must fix. Selection treats such a task as not
// actionable (its dep can never be confirmed done); Validate surfaces it.
type UnknownDepError struct {
	TaskID string // id of the task carrying the bad dep (verbatim)
	Dep    string // the unresolved dep id (verbatim)
}

func (e *UnknownDepError) Error() string {
	return fmt.Sprintf("task %s: depends on unknown task %q", e.TaskID, e.Dep)
}

// DuplicateIDError reports two task files that resolve to the same normalized id
// (e.g. `id: 0007` and `id: 7`). Ids must be unique for deps to resolve, so this
// is rejected at load time rather than silently shadowing one task.
type DuplicateIDError struct {
	ID    string // the normalized id that collided
	Paths [2]string
}

func (e *DuplicateIDError) Error() string {
	return fmt.Sprintf("task: duplicate id %q in %q and %q", e.ID, e.Paths[0], e.Paths[1])
}

// LoadDir enumerates specs/tasks/*.md, parses and validates each, and builds a
// Store. A missing directory is not an error — it just yields an empty store, the
// expected state before the plan loop has run. Each task is validated (spec 02
// frontmatter rules) so the selector only ever reasons over well-formed tasks;
// the first invalid file fails the load with its path.
func LoadDir(dir string) (*Store, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return nil, fmt.Errorf("task: glob %q: %w", dir, err)
	}
	if matches == nil {
		if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
			return NewStore(nil)
		}
	}
	tasks := make([]*Task, 0, len(matches))
	for _, path := range matches {
		t, err := ParseFile(path)
		if err != nil {
			return nil, err
		}
		if err := t.Validate(); err != nil {
			return nil, fmt.Errorf("task: %q: %w", path, err)
		}
		tasks = append(tasks, t)
	}
	return NewStore(tasks)
}

// NewStore builds a Store from already-parsed tasks (the seam tests and the state
// rebuilder use). It indexes by normalized id, rejecting duplicates, and fixes the
// selection order (ascending id) once so Next is deterministic.
func NewStore(tasks []*Task) (*Store, error) {
	s := &Store{
		tasks: append([]*Task(nil), tasks...),
		byID:  make(map[string]*Task, len(tasks)),
	}
	for _, t := range tasks {
		key := normID(t.ID())
		if prev, ok := s.byID[key]; ok {
			return nil, &DuplicateIDError{ID: key, Paths: [2]string{prev.Path, t.Path}}
		}
		s.byID[key] = t
	}
	sort.SliceStable(s.tasks, func(i, j int) bool { return lessID(s.tasks[i], s.tasks[j]) })
	return s, nil
}

// Tasks returns the tasks in selection order (ascending id). The slice is the
// store's own; callers must not mutate it.
func (s *Store) Tasks() []*Task { return s.tasks }

// ByID returns the task with the given id (any zero-padding), or nil. The lookup
// normalizes, so ByID("7"), ByID("0007"), and ByID("07") all find task 0007.
func (s *Store) ByID(id string) *Task { return s.byID[normID(id)] }

// Next returns the next actionable task: the lowest-id `pending` task whose deps
// all resolve to `done` tasks. It returns (nil, nil) when nothing is actionable —
// the plan is finished, or every remaining task is waiting/active/blocked. It
// returns a *CycleError if the dependency graph contains a cycle, because a cycle
// would otherwise masquerade as a finished plan (spec 01 §select, 02 §deps).
func (s *Store) Next() (*Task, error) {
	if err := s.CheckCycles(); err != nil {
		return nil, err
	}
	for _, t := range s.tasks { // already in ascending-id order
		if t.Status() != StatusPending {
			continue
		}
		if s.depsAllDone(t) {
			return t, nil
		}
	}
	return nil, nil
}

// AllDone reports whether every task is done. With Next == nil this is the loop's
// success signal; with Next == nil but AllDone false, the plan is stalled
// (everything left is blocked/waiting) and the orchestrator must intervene.
func (s *Store) AllDone() bool {
	for _, t := range s.tasks {
		if t.Status() != StatusDone {
			return false
		}
	}
	return true
}

// Validate checks the plan's dependency graph as a whole: every dep resolves to a
// real task (UnknownDepError) and there are no cycles (CycleError). Per-task
// frontmatter is already checked at load; this is the cross-task integrity check
// the plan loop runs before handing the plan to the build loop.
func (s *Store) Validate() error {
	for _, t := range s.tasks {
		for _, dep := range t.Deps() {
			if _, ok := s.byID[normID(dep)]; !ok {
				return &UnknownDepError{TaskID: t.ID(), Dep: dep}
			}
		}
	}
	return s.CheckCycles()
}

// CheckCycles reports the first dependency cycle it finds, or nil. It is a
// three-color DFS over the id graph: white (unvisited), gray (on the current
// stack), black (fully explored). Re-entering a gray node closes a cycle, which
// we reconstruct from the active stack. Unknown deps are skipped here — they
// cannot be on a cycle — and are reported separately by Validate.
func (s *Store) CheckCycles() error {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(s.tasks))
	var stack []string

	var visit func(id string) *CycleError
	visit = func(id string) *CycleError {
		color[id] = gray
		stack = append(stack, id)
		for _, dep := range s.byID[id].Deps() {
			d := normID(dep)
			if _, ok := s.byID[d]; !ok {
				continue // unknown dep: not part of any cycle (Validate reports it)
			}
			switch color[d] {
			case gray:
				// Found the back-edge id -> d; the cycle is the stack from d onward.
				start := 0
				for i, n := range stack {
					if n == d {
						start = i
						break
					}
				}
				cyc := append([]string(nil), stack[start:]...)
				cyc = append(cyc, d) // close the loop visually (d -> ... -> d)
				return &CycleError{Cycle: cyc}
			case white:
				if err := visit(d); err != nil {
					return err
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return nil
	}

	// Visit in a stable order so the reported cycle is deterministic.
	for _, t := range s.tasks {
		id := normID(t.ID())
		if color[id] == white {
			if err := visit(id); err != nil {
				return err
			}
		}
	}
	return nil
}

// depsAllDone reports whether every dep of t resolves to a done task. An unknown
// dep counts as not-done: the store can't confirm it, so t is not selected — the
// loop never runs a task whose deps aren't provably satisfied (spec 02).
func (s *Store) depsAllDone(t *Task) bool {
	for _, dep := range t.Deps() {
		d, ok := s.byID[normID(dep)]
		if !ok || d.Status() != StatusDone {
			return false
		}
	}
	return true
}

// normID is the canonical key for an id. Ids are stored verbatim to preserve
// zero-padding on disk, so comparison must collapse `0007`/`7`/`07` to one key:
// trim surrounding space, then strip leading zeros from an all-digit id (keeping a
// lone "0"). Non-numeric ids are compared as their trimmed selves.
func normID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || !isAllDigits(id) {
		return id
	}
	trimmed := strings.TrimLeft(id, "0")
	if trimmed == "" {
		return "0"
	}
	return trimmed
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// lessID orders tasks for selection: numerically when both ids are integers (so 2
// sorts before 10), lexicographically otherwise. Selection ("lowest-id eligible")
// depends on this being a total, stable order.
func lessID(a, b *Task) bool {
	ai, aerr := strconv.Atoi(strings.TrimSpace(a.ID()))
	bi, berr := strconv.Atoi(strings.TrimSpace(b.ID()))
	if aerr == nil && berr == nil {
		return ai < bi
	}
	return a.ID() < b.ID()
}
