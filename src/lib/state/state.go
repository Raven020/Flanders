// Package state is the harness's resumable cursor and run-state cache —
// .flanders/state.json (specs/09-state-and-resume.md).
//
// Why this is a *cache* and not a store: Flanders is Ralph, so all durable truth
// lives in files + git, never in process memory (spec 09 §state hierarchy). The
// task files (specs/tasks/*.md) own status; the git history records what was
// actually done; the test command is the done-gate. state.json sits *below* those
// in authority — it exists only so a restart resumes instantly without
// re-deriving, and so the TUI can repaint the run state after a close/reopen. If
// it is missing or corrupt, that is a cache miss, not an error: the harness
// reconstructs a fresh snapshot from the ground-truth tier (Rebuild) and carries
// on. This is exactly what makes unattended multi-day runs and "close/reopen
// resumes" safe.
//
// Why writes are atomic (temp + rename): the harness writes state.json at every
// transition (loop start/end, status change, checkpoint, guardrail trip, usage
// wait/resume). A crash mid-write must never leave a half-written file that fails
// to parse on the next start — that would turn a routine restart into a forced
// rebuild. The rename is atomic on POSIX, so a reader sees either the old
// snapshot or the new one, never a torn one.
//
// Why only the harness writes it: the agent must never hand-edit harness-owned
// state (spec 01). The agent's structured surface is the task file's `status`;
// everything here is the harness's bookkeeping.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"flanders/src/lib/task"
)

// SchemaVersion is the on-disk schema generation. It is written into every
// snapshot and checked on load: a file carrying a different version is treated as
// unreadable (a *CorruptError) so the harness rebuilds from ground truth rather
// than misinterpreting fields it does not understand. Migration policy when this
// bumps is OPEN (spec 09) — rebuild-from-truth is the safe default until then.
const SchemaVersion = 1

// Phase is the pipeline stage the run is in (spec 06 §phases). `orchestrate` is
// the bare `flanders` command coordinating plan→build.
type Phase string

const (
	PhaseDiscuss     Phase = "discuss"
	PhasePlan        Phase = "plan"
	PhaseBuild       Phase = "build"
	PhaseOrchestrate Phase = "orchestrate"
)

// RunState is the coarse run status the TUI header renders (spec 04). It is the
// resume hint, too: WAITING re-enters the usage countdown, RUNNING means a crash
// mid-loop that must be reconciled against ground truth, HALTED/PAUSED/DONE
// restore as-is (spec 09 §resume).
type RunState string

const (
	StateRunning RunState = "RUNNING"
	StatePaused  RunState = "PAUSED"
	StateWaiting RunState = "WAITING"
	StateHalted  RunState = "HALTED"
	StateDone    RunState = "DONE"
)

// Iter holds per-phase and total iteration counts, checked against the
// max-iterations guardrail (spec 01/03). Total is tracked explicitly rather than
// summed so a future phase the counters don't enumerate still contributes.
type Iter struct {
	Plan  int `json:"plan"`
	Build int `json:"build"`
	Total int `json:"total"`
}

// Stall is the consecutive-no-change counter vs. the configured limit
// (guardrails.stall_n). N mirrors config so the TUI can render `k/N` without a
// second lookup; Rebuild leaves it 0 for the orchestrator to fill from config.
type Stall struct {
	Count int `json:"count"`
	N     int `json:"n"`
}

// Usage tracks the subscription usage-window wait (spec 00/01). ResetAt is a
// pointer so a missing/unparseable reset serializes to JSON null, which the
// resume path reads as "fall back to [usage].backoff."
type Usage struct {
	Waiting    bool       `json:"waiting"`
	ResetAt    *time.Time `json:"reset_at"`
	CyclesUsed int        `json:"cycles_used"`
}

// Halt records why a guardrail stopped the run; populated only when RunState is
// HALTED, restored for the recovery UX (spec 06, OPEN).
type Halt struct {
	Reason string `json:"reason"`
	Task   string `json:"task"`
}

// State is the full state.json snapshot. Field set follows the proposal in spec
// 09; it is marked OPEN there, so unknown future keys are tolerated on read by
// json's default (extra keys ignored) — but because this is a derived cache, the
// safe response to anything we cannot interpret is to rebuild, not to guess.
type State struct {
	SchemaVersion int       `json:"schema_version"`
	Phase         Phase     `json:"phase"`
	RunState      RunState  `json:"run_state"`
	StartedAt     time.Time `json:"started_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	Iter        Iter   `json:"iter"`
	CurrentTask string `json:"current_task"`

	Stall Stall `json:"stall"`
	Usage Usage `json:"usage"`

	Halt           Halt   `json:"halt"`
	LastCheckpoint string `json:"last_checkpoint"`
	LastSessionID  string `json:"last_session_id"`
}

// CorruptError marks a state.json that exists but cannot be trusted — unparseable
// JSON or an unrecognized schema_version. It is a recoverable condition: callers
// (LoadOrRebuild) respond by reconstructing from ground truth, not by failing.
type CorruptError struct {
	Path string
	Err  error
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("state: %q is corrupt: %v", e.Path, e.Err)
}

func (e *CorruptError) Unwrap() error { return e.Err }

// New returns a fresh RUNNING snapshot for phase, with started/updated stamped to
// now. It is the "first launch" state before any task is selected.
func New(phase Phase) *State {
	now := time.Now().UTC()
	return &State{
		SchemaVersion: SchemaVersion,
		Phase:         phase,
		RunState:      StateRunning,
		StartedAt:     now,
		UpdatedAt:     now,
	}
}

// Validate checks the structural invariants that make a snapshot trustworthy: a
// known schema, valid phase/run-state enums, and non-negative counters. It does
// *not* enforce cross-field semantics (e.g. WAITING⇒usage.waiting) — those are
// maintained by transition helpers; Validate is the gate Load uses to decide
// whether a file is corrupt, so it stays to integrity checks only.
func (s *State) Validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version %d (want %d)", s.SchemaVersion, SchemaVersion)
	}
	switch s.Phase {
	case PhaseDiscuss, PhasePlan, PhaseBuild, PhaseOrchestrate:
	default:
		return fmt.Errorf("invalid phase %q", s.Phase)
	}
	switch s.RunState {
	case StateRunning, StatePaused, StateWaiting, StateHalted, StateDone:
	default:
		return fmt.Errorf("invalid run_state %q", s.RunState)
	}
	if s.Iter.Plan < 0 || s.Iter.Build < 0 || s.Iter.Total < 0 {
		return fmt.Errorf("negative iteration count %+v", s.Iter)
	}
	if s.Stall.Count < 0 || s.Stall.N < 0 {
		return fmt.Errorf("negative stall count %+v", s.Stall)
	}
	if s.Usage.CyclesUsed < 0 {
		return fmt.Errorf("negative usage.cycles_used %d", s.Usage.CyclesUsed)
	}
	return nil
}

// Save writes the snapshot to path atomically (temp file in the same directory +
// rename), creating the parent directory if needed. It stamps UpdatedAt to now
// first, so "save on every transition" doubles as the heartbeat the TUI reads —
// every persisted state carries the moment it was persisted. The temp file lives
// in the destination directory so the rename stays on one filesystem (atomic).
func (s *State) Save(path string) error {
	if path == "" {
		return errors.New("state: Save needs a path")
	}
	s.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("state: create %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("state: create temp in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("state: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("state: rename temp to %q: %w", path, err)
	}
	return nil
}

// Load reads and validates a snapshot. It distinguishes three outcomes so callers
// can react precisely: a missing file returns an error wrapping os.ErrNotExist; a
// present-but-unreadable file (bad JSON or unknown schema) returns a
// *CorruptError; any other I/O failure is returned verbatim. Both of the first
// two are recoverable via Rebuild — see LoadOrRebuild, the startup entry point.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err // includes os.ErrNotExist; callers test with errors.Is
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, &CorruptError{Path: path, Err: err}
	}
	if err := s.Validate(); err != nil {
		return nil, &CorruptError{Path: path, Err: err}
	}
	return &s, nil
}

// Rebuild reconstructs a fresh snapshot from ground truth when state.json is
// missing or corrupt (spec 09 §recovery). Today the only ground-truth tier that
// exists is the task store, so Rebuild derives the cursor from it: it points
// CurrentTask at the task an interrupted loop most likely left behind — an
// `active` task if one exists (a crash mid-loop), otherwise the next actionable
// task — and defaults RunState to RUNNING for the given phase.
//
// What it deliberately leaves zero: Iter/Stall.N/Usage are config- and
// journal-derived (the journal is tier 2, not built yet — 1.6; the git checkpoint
// is 3.6). The orchestrator overlays those from config and the journal on
// startup. Leaving them zero is honest: a rebuilt cache claims only what ground
// truth can currently prove — which task to resume on — not fabricated counters.
func Rebuild(store *task.Store, phase Phase) *State {
	s := New(phase)
	if store == nil {
		return s
	}
	// Prefer an active task: if a previous loop crashed, the agent (or harness)
	// would have flipped its status to `active` before work landed.
	for _, t := range store.Tasks() {
		if t.Status() == task.StatusActive {
			s.CurrentTask = t.ID()
			return s
		}
	}
	// Otherwise resume at the next actionable task. A cycle/selection error is not
	// fatal here — Rebuild's job is a best-effort cursor; the plan loop surfaces a
	// broken graph. So we ignore the error and leave CurrentTask empty.
	if next, err := store.Next(); err == nil && next != nil {
		s.CurrentTask = next.ID()
	}
	return s
}

// LoadOrRebuild is the startup entry point (spec 09 §recovery). It loads the
// snapshot if present and trustworthy; on a missing or corrupt file it rebuilds
// from the task store and reports rebuilt=true so the caller can log the cache
// miss. fallbackPhase is the phase the launching command intends (e.g. `build`),
// used only when rebuilding. Any non-recoverable I/O error (e.g. a permission
// failure that is not "not found") is returned as-is — that is a real problem,
// not a cache miss.
func LoadOrRebuild(path string, store *task.Store, fallbackPhase Phase) (st *State, rebuilt bool, err error) {
	s, loadErr := Load(path)
	if loadErr == nil {
		return s, false, nil
	}
	var corrupt *CorruptError
	if errors.Is(loadErr, os.ErrNotExist) || errors.As(loadErr, &corrupt) {
		return Rebuild(store, fallbackPhase), true, nil
	}
	return nil, false, loadErr
}
