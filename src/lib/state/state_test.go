package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"flanders/src/lib/task"
)

// sampleState returns a fully-populated snapshot exercising every field,
// including the pointer ResetAt, so the round-trip test proves nothing is lost.
func sampleState() *State {
	reset := time.Date(2026, 5, 25, 18, 24, 0, 0, time.UTC)
	return &State{
		SchemaVersion:  SchemaVersion,
		Phase:          PhaseBuild,
		RunState:       StateWaiting,
		StartedAt:      time.Date(2026, 5, 25, 14, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2026, 5, 25, 15, 12, 0, 0, time.UTC),
		Iter:           Iter{Plan: 3, Build: 12, Total: 15},
		CurrentTask:    "0007",
		Stall:          Stall{Count: 0, N: 3},
		Usage:          Usage{Waiting: true, ResetAt: &reset, CyclesUsed: 1},
		Halt:           Halt{},
		LastCheckpoint: "3cb3837",
		LastSessionID:  "sess-abc",
	}
}

// Round-trip is the headline acceptance for 1.5: save then load yields an equal
// snapshot. Save mutates UpdatedAt (the heartbeat), so we compare against the
// in-memory value after Save, not the pre-Save literal.
func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".flanders", "state.json")
	want := sampleState()
	if err := want.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// time.Time round-trips through RFC3339; compare via marshaling to dodge
	// monotonic-clock / location wrinkles in reflect.DeepEqual on times.
	if !jsonEqual(t, want, got) {
		wb, _ := json.Marshal(want)
		gb, _ := json.Marshal(got)
		t.Fatalf("round-trip mismatch:\n want %s\n got  %s", wb, gb)
	}
}

// Save writes atomically and stamps UpdatedAt; verify the heartbeat advances and
// the file is valid JSON ending in a newline (human-inspectable).
func TestSaveStampsUpdatedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := New(PhasePlan)
	before := s.UpdatedAt
	time.Sleep(2 * time.Millisecond)
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !s.UpdatedAt.After(before) {
		t.Fatalf("UpdatedAt not advanced: before=%v after=%v", before, s.UpdatedAt)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatalf("expected trailing newline")
	}
	var probe map[string]any
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatalf("file is not valid JSON: %v", err)
	}
}

// Save creates the parent directory on demand (.flanders may not exist yet).
func TestSaveCreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep", "nested", "state.json")
	if err := New(PhaseBuild).Save(path); err != nil {
		t.Fatalf("Save into missing dirs: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state not written: %v", err)
	}
}

// A missing file is a cache miss, distinguishable via errors.Is(os.ErrNotExist),
// not a CorruptError.
func TestLoadMissingIsNotExist(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
	var corrupt *CorruptError
	if errors.As(err, &corrupt) {
		t.Fatalf("missing file should not be CorruptError")
	}
}

// Corrupt JSON and an unknown schema both surface as *CorruptError without
// crashing — the recoverable signal LoadOrRebuild keys off.
func TestLoadCorrupt(t *testing.T) {
	cases := map[string]string{
		"bad json":      "{not json",
		"empty":         "",
		"wrong schema":  `{"schema_version": 999, "phase":"build", "run_state":"RUNNING"}`,
		"bad run_state": `{"schema_version": 1, "phase":"build", "run_state":"NOPE"}`,
		"bad phase":     `{"schema_version": 1, "phase":"frobnicate", "run_state":"RUNNING"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			var corrupt *CorruptError
			if !errors.As(err, &corrupt) {
				t.Fatalf("want *CorruptError, got %v", err)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	if err := sampleState().Validate(); err != nil {
		t.Fatalf("valid state rejected: %v", err)
	}
	bad := sampleState()
	bad.Iter.Total = -1
	if err := bad.Validate(); err == nil {
		t.Fatal("negative iteration count accepted")
	}
}

// Rebuild points the cursor at an `active` task — the one a crash mid-loop would
// have left behind — in preference to the next actionable task.
func TestRebuildPrefersActiveTask(t *testing.T) {
	store := mustStore(t,
		task.New("1", task.StatusDone, nil, "done crit", ""),
		task.New("2", task.StatusActive, []string{"1"}, "active crit", ""),
		task.New("3", task.StatusPending, []string{"1"}, "pending crit", ""),
	)
	s := Rebuild(store, PhaseBuild)
	if s.RunState != StateRunning {
		t.Fatalf("rebuilt run_state = %q, want RUNNING", s.RunState)
	}
	if s.Phase != PhaseBuild {
		t.Fatalf("rebuilt phase = %q, want build", s.Phase)
	}
	if s.CurrentTask != "2" {
		t.Fatalf("rebuilt current_task = %q, want active task 2", s.CurrentTask)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("rebuilt state invalid: %v", err)
	}
}

// With no active task, Rebuild resumes at the next actionable task (deps done).
func TestRebuildNextActionable(t *testing.T) {
	store := mustStore(t,
		task.New("1", task.StatusDone, nil, "done crit", ""),
		task.New("2", task.StatusPending, []string{"1"}, "ready crit", ""),
		task.New("3", task.StatusPending, []string{"99"}, "blocked-by-missing", ""),
	)
	s := Rebuild(store, PhaseBuild)
	if s.CurrentTask != "2" {
		t.Fatalf("current_task = %q, want next-actionable 2", s.CurrentTask)
	}
}

// A nil store (no plan yet) rebuilds a valid empty-cursor snapshot, never panics.
func TestRebuildNilStore(t *testing.T) {
	s := Rebuild(nil, PhasePlan)
	if s.CurrentTask != "" || s.Phase != PhasePlan {
		t.Fatalf("unexpected rebuilt state %+v", s)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("invalid: %v", err)
	}
}

func TestLoadOrRebuild(t *testing.T) {
	store := mustStore(t,
		task.New("1", task.StatusPending, nil, "crit", ""),
	)

	t.Run("missing rebuilds", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		s, rebuilt, err := LoadOrRebuild(path, store, PhaseBuild)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !rebuilt {
			t.Fatal("expected rebuilt=true for missing file")
		}
		if s.CurrentTask != "1" {
			t.Fatalf("current_task = %q, want 1", s.CurrentTask)
		}
	})

	t.Run("corrupt rebuilds", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		if err := os.WriteFile(path, []byte("{garbage"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, rebuilt, err := LoadOrRebuild(path, store, PhaseBuild)
		if err != nil {
			t.Fatalf("corrupt should recover, got err: %v", err)
		}
		if !rebuilt {
			t.Fatal("expected rebuilt=true for corrupt file")
		}
	})

	t.Run("present loads as-is", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		want := sampleState()
		if err := want.Save(path); err != nil {
			t.Fatal(err)
		}
		got, rebuilt, err := LoadOrRebuild(path, store, PhaseBuild)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if rebuilt {
			t.Fatal("expected rebuilt=false for a valid file")
		}
		if got.CurrentTask != "0007" || got.RunState != StateWaiting {
			t.Fatalf("loaded snapshot not preserved: %+v", got)
		}
	})
}

// --- helpers ---

func mustStore(t *testing.T, tasks ...*task.Task) *task.Store {
	t.Helper()
	s, err := task.NewStore(tasks)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	ab, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(ab) == string(bb)
}
