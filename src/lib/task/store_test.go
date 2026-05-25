package task

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// mk builds a minimal valid task for store tests. deps are wired via New; status
// is set after construction so blocked tasks get a reason (New takes none).
func mk(id string, status Status, deps ...string) *Task {
	t := New(id, status, deps, "acc-"+id, "## "+id+"\n")
	if status == StatusBlocked {
		t.SetBlocked(ReasonDependency)
	}
	return t
}

func mustStore(t *testing.T, tasks ...*Task) *Store {
	t.Helper()
	s, err := NewStore(tasks)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// nextID is a tiny helper: the id of Next, or "" for nil, failing on error.
func nextID(t *testing.T, s *Store) string {
	t.Helper()
	n, err := s.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if n == nil {
		return ""
	}
	return n.ID()
}

// TestLoadDirEnumerates writes a few task files (plus a non-.md decoy) and checks
// the store reads exactly the .md tasks and indexes them.
func TestLoadDirEnumerates(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("0001-a.md", "---\nid: 0001\nstatus: done\nacceptance: \"a\"\n---\nbody\n")
	write("0007-b.md", "---\nid: 0007\nstatus: pending\ndeps: [0001]\nacceptance: \"b\"\n---\nbody\n")
	write("README.txt", "not a task")

	s, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(s.Tasks()) != 2 {
		t.Fatalf("Tasks() len = %d, want 2", len(s.Tasks()))
	}
	if s.ByID("1") == nil || s.ByID("0001") == nil {
		t.Errorf("ByID did not normalize zero-padding for id 0001")
	}
	if got := nextID(t, s); got != "0007" {
		t.Errorf("Next = %q, want 0007 (its only dep 0001 is done)", got)
	}
}

// TestLoadDirMissingIsEmpty: before the plan loop runs, specs/tasks/ may not
// exist. That must be an empty store, not an error.
func TestLoadDirMissingIsEmpty(t *testing.T) {
	s, err := LoadDir(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("LoadDir(missing) = %v, want nil", err)
	}
	if len(s.Tasks()) != 0 {
		t.Errorf("Tasks() len = %d, want 0", len(s.Tasks()))
	}
	if id := nextID(t, s); id != "" {
		t.Errorf("Next on empty store = %q, want \"\"", id)
	}
	if !s.AllDone() {
		t.Errorf("AllDone on empty store = false, want true (vacuously)")
	}
}

// TestLoadDirRejectsInvalid: a malformed task file fails the whole load so the
// selector never reasons over bad data.
func TestLoadDirRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	// blocked without a reason — invalid per spec 02.
	bad := "---\nid: 1\nstatus: blocked\nacceptance: \"a\"\n---\n"
	if err := os.WriteFile(filepath.Join(dir, "0001-bad.md"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(dir); err == nil {
		t.Errorf("LoadDir with invalid task = nil error, want error")
	}
}

// TestNextSelectsLowestEligible: among several actionable pending tasks, the
// lowest id wins, and 2 sorts before 10 (numeric, not lexicographic).
func TestNextSelectsLowestEligible(t *testing.T) {
	s := mustStore(t,
		mk("10", StatusPending),
		mk("2", StatusPending),
		mk("3", StatusDone),
	)
	if got := nextID(t, s); got != "2" {
		t.Errorf("Next = %q, want 2 (lowest eligible, numeric order)", got)
	}
}

// TestNextSkipsUnmetDeps covers every way a dep can be unsatisfied: a pending
// dep, a blocked dep, and an unknown dep. None should be selected; the one whose
// deps are all done is.
func TestNextSkipsUnmetDeps(t *testing.T) {
	s := mustStore(t,
		mk("1", StatusDone),
		mk("2", StatusPending, "1"),         // eligible: dep 1 is done
		mk("3", StatusPending, "2"),         // not yet: dep 2 still pending
		mk("4", StatusPending, "999"),       // unknown dep: never eligible
		mk("5", StatusBlocked),              // blocked: dep target, not selectable
		mk("6", StatusPending, "5"),         // dep is blocked, not done
	)
	if got := nextID(t, s); got != "2" {
		t.Errorf("Next = %q, want 2", got)
	}
	// Flip 2 to done; now 3 becomes the only newly-eligible one.
	s.ByID("2").SetStatus(StatusDone)
	if got := nextID(t, s); got != "3" {
		t.Errorf("after 2 done, Next = %q, want 3", got)
	}
}

// TestNextZeroPaddedDeps is the headline normalization case: a task's dep is
// written `0001` while the task it names has id `1` (no padding). They must match.
func TestNextZeroPaddedDeps(t *testing.T) {
	s := mustStore(t,
		mk("1", StatusDone),               // id "1"
		mk("0007", StatusPending, "0001"), // dep "0001" must resolve to task "1"
	)
	if got := nextID(t, s); got != "0007" {
		t.Errorf("Next = %q, want 0007 (dep 0001 resolves to done task 1)", got)
	}
}

// TestNextNoneWhenAllDone / stalled: distinguish "finished" from "wedged".
func TestNextNoneAndAllDone(t *testing.T) {
	done := mustStore(t, mk("1", StatusDone), mk("2", StatusDone))
	if id := nextID(t, done); id != "" {
		t.Errorf("Next = %q, want \"\" (all done)", id)
	}
	if !done.AllDone() {
		t.Errorf("AllDone = false, want true")
	}

	stalled := mustStore(t, mk("1", StatusBlocked), mk("2", StatusPending, "1"))
	if id := nextID(t, stalled); id != "" {
		t.Errorf("Next = %q, want \"\" (blocked dep stalls 2)", id)
	}
	if stalled.AllDone() {
		t.Errorf("AllDone = true, want false (1 is blocked)")
	}
}

// TestCycleDetected: a two-node cycle (1<->2) must surface as *CycleError, not a
// silent nil that looks like a finished plan.
func TestCycleDetected(t *testing.T) {
	s := mustStore(t,
		mk("1", StatusPending, "2"),
		mk("2", StatusPending, "1"),
	)
	_, err := s.Next()
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("Next = %v, want *CycleError", err)
	}
	if len(ce.Cycle) < 2 {
		t.Errorf("CycleError.Cycle = %v, want the loop ids", ce.Cycle)
	}
}

// TestSelfCycle: a task depending on itself is a degenerate cycle.
func TestSelfCycle(t *testing.T) {
	s := mustStore(t, mk("1", StatusPending, "1"))
	if _, err := s.Next(); err == nil {
		t.Errorf("Next with self-dependency = nil, want *CycleError")
	}
}

// TestCycleZeroPadded: cycle detection must also normalize ids (0002 -> 1, 0001 -> 2).
func TestCycleZeroPadded(t *testing.T) {
	s := mustStore(t,
		mk("0001", StatusPending, "0002"),
		mk("0002", StatusPending, "0001"),
	)
	if _, err := s.Next(); err == nil {
		t.Errorf("Next = nil, want *CycleError across zero-padded ids")
	}
}

// TestNoFalseCycle: a diamond (4 deps on 2 and 3, both dep on 1) is a DAG, not a
// cycle — shared deps must not be mistaken for a loop.
func TestNoFalseCycle(t *testing.T) {
	s := mustStore(t,
		mk("1", StatusDone),
		mk("2", StatusDone, "1"),
		mk("3", StatusDone, "1"),
		mk("4", StatusPending, "2", "3"),
	)
	if got := nextID(t, s); got != "4" {
		t.Errorf("Next = %q, want 4 (diamond DAG, no cycle)", got)
	}
}

// TestValidateUnknownDep: Validate flags a dep that names no task.
func TestValidateUnknownDep(t *testing.T) {
	s := mustStore(t, mk("1", StatusPending, "999"))
	err := s.Validate()
	var ue *UnknownDepError
	if !errors.As(err, &ue) {
		t.Fatalf("Validate = %v, want *UnknownDepError", err)
	}
	if ue.Dep != "999" {
		t.Errorf("UnknownDepError.Dep = %q, want 999", ue.Dep)
	}
}

// TestDuplicateID: two files resolving to the same normalized id is rejected.
func TestDuplicateID(t *testing.T) {
	_, err := NewStore([]*Task{mk("0007", StatusPending), mk("7", StatusPending)})
	var de *DuplicateIDError
	if !errors.As(err, &de) {
		t.Fatalf("NewStore with duplicate id = %v, want *DuplicateIDError", err)
	}
}
