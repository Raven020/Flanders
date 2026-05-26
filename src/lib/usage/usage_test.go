package usage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"flanders/src/lib/config"
	"flanders/src/lib/state"
)

func waitCfg(maxCycles int, backoff time.Duration) config.Usage {
	return config.Usage{OnLimit: "wait", MaxCycles: maxCycles, Backoff: config.Duration{Duration: backoff}}
}

// --- Decide (pure) --------------------------------------------------------

func TestDecideWaitUnderCap(t *testing.T) {
	dec := Decide(waitCfg(0, 30*time.Minute), 5)
	if dec.Action != ActionWait {
		t.Fatalf("on_limit=wait, max_cycles=0 (unlimited): got %v, want ActionWait", dec.Action)
	}
}

func TestDecideHaltOnLimit(t *testing.T) {
	dec := Decide(config.Usage{OnLimit: "halt"}, 0)
	if dec.Action != ActionHalt {
		t.Fatalf("on_limit=halt: got %v, want ActionHalt", dec.Action)
	}
	if dec.HaltReason == "" {
		t.Fatal("halt decision must carry a reason")
	}
}

func TestDecideHaltOnLimitCaseInsensitive(t *testing.T) {
	if dec := Decide(config.Usage{OnLimit: " HALT "}, 0); dec.Action != ActionHalt {
		t.Fatalf("on_limit=' HALT ' should halt (trim+fold): got %v", dec.Action)
	}
}

func TestDecideHaltAtMaxCycles(t *testing.T) {
	// max_cycles=2 drains exactly 2 windows; the 3rd limit (cycles_used==2) halts.
	if dec := Decide(waitCfg(2, 0), 0); dec.Action != ActionWait {
		t.Fatalf("1st window (cycles_used=0 < 2): got %v, want ActionWait", dec.Action)
	}
	if dec := Decide(waitCfg(2, 0), 1); dec.Action != ActionWait {
		t.Fatalf("2nd window (cycles_used=1 < 2): got %v, want ActionWait", dec.Action)
	}
	dec := Decide(waitCfg(2, 0), 2)
	if dec.Action != ActionHalt {
		t.Fatalf("3rd window (cycles_used=2 >= 2): got %v, want ActionHalt", dec.Action)
	}
	if dec.HaltReason == "" {
		t.Fatal("max_cycles halt must carry a reason")
	}
}

func TestDecideUnlimitedNeverHaltsOnCycles(t *testing.T) {
	if dec := Decide(waitCfg(0, 0), 10_000); dec.Action != ActionWait {
		t.Fatalf("max_cycles=0 must never halt on cycle count: got %v", dec.Action)
	}
}

// --- WaitDuration (pure) --------------------------------------------------

func TestWaitDurationResetInFuture(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	reset := now.Add(45 * time.Minute)
	d, usedBackoff := WaitDuration(&reset, 30*time.Minute, now)
	if d != 45*time.Minute {
		t.Fatalf("future reset: got %v, want 45m", d)
	}
	if usedBackoff {
		t.Fatal("a parsed future reset must not be reported as backoff")
	}
}

func TestWaitDurationResetInPast(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	reset := now.Add(-time.Minute)
	d, usedBackoff := WaitDuration(&reset, 30*time.Minute, now)
	if d != 0 {
		t.Fatalf("past reset must resume immediately: got %v, want 0", d)
	}
	if usedBackoff {
		t.Fatal("past reset is still a parsed reset, not backoff")
	}
}

func TestWaitDurationBackoffWhenNil(t *testing.T) {
	now := time.Now()
	d, usedBackoff := WaitDuration(nil, 30*time.Minute, now)
	if d != 30*time.Minute {
		t.Fatalf("nil reset must fall back to backoff: got %v, want 30m", d)
	}
	if !usedBackoff {
		t.Fatal("nil reset must report used_backoff=true")
	}
}

func TestWaitDurationNegativeBackoffClamped(t *testing.T) {
	if d, _ := WaitDuration(nil, -5*time.Minute, time.Now()); d != 0 {
		t.Fatalf("negative backoff must clamp to 0: got %v", d)
	}
}

// --- HandleLimit: wait → resume ------------------------------------------

// fixedClock + recordingSleep make the wait deterministic and inspectable.
type harness struct {
	t         *testing.T
	statePath string
	now       time.Time
	slept     []time.Duration
	onSleep   func() // invoked inside the fake sleep, before it returns
	sleepErr  error  // returned by the fake sleep (e.g. context cancellation)
}

func newWaiter(t *testing.T, cfg config.Usage) (*Waiter, *harness) {
	t.Helper()
	h := &harness{
		t:         t,
		statePath: filepath.Join(t.TempDir(), "state.json"),
		now:       time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC),
	}
	w := NewWaiter(cfg, nil)
	w.now = func() time.Time { return h.now }
	w.sleep = func(ctx context.Context, d time.Duration) error {
		h.slept = append(h.slept, d)
		if h.onSleep != nil {
			h.onSleep()
		}
		return h.sleepErr
	}
	return w, h
}

func freshState() *state.State {
	st := state.New(state.PhaseBuild)
	st.CurrentTask = "0007"
	return st
}

func TestHandleLimitWaitsThenResumes(t *testing.T) {
	w, h := newWaiter(t, waitCfg(0, 30*time.Minute))
	st := freshState()
	reset := h.now.Add(time.Hour)

	// When the harness sleeps, the on-disk state must already say WAITING with the
	// incremented cycle counter — the persist-before-sleep crash guarantee.
	h.onSleep = func() {
		onDisk, err := state.Load(h.statePath)
		if err != nil {
			t.Fatalf("state not persisted before sleep: %v", err)
		}
		if onDisk.RunState != state.StateWaiting {
			t.Fatalf("on-disk run_state before sleep = %q, want WAITING", onDisk.RunState)
		}
		if !onDisk.Usage.Waiting {
			t.Fatal("on-disk usage.waiting must be true during the wait")
		}
		if onDisk.Usage.CyclesUsed != 1 {
			t.Fatalf("on-disk cycles_used before sleep = %d, want 1", onDisk.Usage.CyclesUsed)
		}
		if onDisk.Usage.ResetAt == nil || !onDisk.Usage.ResetAt.Equal(reset) {
			t.Fatalf("on-disk reset_at = %v, want %v", onDisk.Usage.ResetAt, reset)
		}
	}

	dec, err := w.HandleLimit(context.Background(), st, h.statePath, &reset)
	if err != nil {
		t.Fatalf("HandleLimit: %v", err)
	}
	if dec.Action != ActionWait {
		t.Fatalf("decision = %v, want ActionWait", dec.Action)
	}
	if len(h.slept) != 1 || h.slept[0] != time.Hour {
		t.Fatalf("slept = %v, want exactly [1h]", h.slept)
	}
	// After resume the in-memory and on-disk state must be RUNNING, not waiting.
	if st.RunState != state.StateRunning || st.Usage.Waiting {
		t.Fatalf("after resume: run_state=%q waiting=%v, want RUNNING/false", st.RunState, st.Usage.Waiting)
	}
	onDisk, err := state.Load(h.statePath)
	if err != nil {
		t.Fatalf("load after resume: %v", err)
	}
	if onDisk.RunState != state.StateRunning || onDisk.Usage.Waiting {
		t.Fatalf("on-disk after resume: run_state=%q waiting=%v, want RUNNING/false", onDisk.RunState, onDisk.Usage.Waiting)
	}
	if onDisk.Usage.CyclesUsed != 1 {
		t.Fatalf("on-disk cycles_used after resume = %d, want 1", onDisk.Usage.CyclesUsed)
	}
}

func TestHandleLimitUsesBackoffWhenNoReset(t *testing.T) {
	w, h := newWaiter(t, waitCfg(0, 30*time.Minute))
	st := freshState()

	if _, err := w.HandleLimit(context.Background(), st, h.statePath, nil); err != nil {
		t.Fatalf("HandleLimit: %v", err)
	}
	if len(h.slept) != 1 || h.slept[0] != 30*time.Minute {
		t.Fatalf("slept = %v, want exactly [30m] (backoff)", h.slept)
	}
}

func TestHandleLimitHaltMode(t *testing.T) {
	w, h := newWaiter(t, config.Usage{OnLimit: "halt"})
	st := freshState()

	dec, err := w.HandleLimit(context.Background(), st, h.statePath, nil)
	if err != nil {
		t.Fatalf("HandleLimit: %v", err)
	}
	if dec.Action != ActionHalt {
		t.Fatalf("decision = %v, want ActionHalt", dec.Action)
	}
	if len(h.slept) != 0 {
		t.Fatalf("halt mode must not sleep: slept=%v", h.slept)
	}
	if st.RunState != state.StateHalted {
		t.Fatalf("run_state = %q, want HALTED", st.RunState)
	}
	if st.Halt.Reason == "" || st.Halt.Task != "0007" {
		t.Fatalf("halt = %+v, want a reason and task 0007", st.Halt)
	}
	onDisk, err := state.Load(h.statePath)
	if err != nil {
		t.Fatalf("load after halt: %v", err)
	}
	if onDisk.RunState != state.StateHalted || onDisk.Halt.Reason == "" {
		t.Fatalf("on-disk halt not persisted: %+v", onDisk)
	}
}

func TestHandleLimitHaltAtMaxCycles(t *testing.T) {
	w, h := newWaiter(t, waitCfg(1, 30*time.Minute))
	st := freshState()
	st.Usage.CyclesUsed = 1 // already drained the one allowed window

	dec, err := w.HandleLimit(context.Background(), st, h.statePath, nil)
	if err != nil {
		t.Fatalf("HandleLimit: %v", err)
	}
	if dec.Action != ActionHalt {
		t.Fatalf("decision = %v, want ActionHalt (max_cycles reached)", dec.Action)
	}
	if len(h.slept) != 0 {
		t.Fatal("max_cycles halt must not sleep")
	}
	if st.RunState != state.StateHalted {
		t.Fatalf("run_state = %q, want HALTED", st.RunState)
	}
}

func TestHandleLimitContextCancelledStaysWaiting(t *testing.T) {
	w, h := newWaiter(t, waitCfg(0, 30*time.Minute))
	h.sleepErr = context.Canceled
	st := freshState()
	reset := h.now.Add(time.Hour)

	_, err := w.HandleLimit(context.Background(), st, h.statePath, &reset)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("HandleLimit err = %v, want context.Canceled", err)
	}
	// A cancelled wait must leave WAITING on disk so a restart resumes it.
	onDisk, loadErr := state.Load(h.statePath)
	if loadErr != nil {
		t.Fatalf("load after cancel: %v", loadErr)
	}
	if onDisk.RunState != state.StateWaiting || !onDisk.Usage.Waiting {
		t.Fatalf("after cancel: run_state=%q waiting=%v, want WAITING/true", onDisk.RunState, onDisk.Usage.Waiting)
	}
	if onDisk.Usage.CyclesUsed != 1 {
		t.Fatalf("cycles_used after cancel = %d, want 1 (window was claimed)", onDisk.Usage.CyclesUsed)
	}
}

// --- Resume: crash-resume path -------------------------------------------

func TestResumeSleepsRemainingThenRuns(t *testing.T) {
	w, h := newWaiter(t, waitCfg(0, 30*time.Minute))
	// Simulate the persisted WAITING snapshot a prior process left behind.
	st := freshState()
	st.RunState = state.StateWaiting
	st.Usage.Waiting = true
	st.Usage.CyclesUsed = 3
	reset := h.now.Add(20 * time.Minute)
	st.Usage.ResetAt = &reset

	if err := w.Resume(context.Background(), st, h.statePath); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(h.slept) != 1 || h.slept[0] != 20*time.Minute {
		t.Fatalf("slept = %v, want [20m] (remaining to reset)", h.slept)
	}
	if st.RunState != state.StateRunning || st.Usage.Waiting {
		t.Fatalf("after resume: run_state=%q waiting=%v, want RUNNING/false", st.RunState, st.Usage.Waiting)
	}
	if st.Usage.CyclesUsed != 3 {
		t.Fatalf("Resume must not touch cycles_used: got %d, want 3", st.Usage.CyclesUsed)
	}
}

func TestResumePastResetRunsImmediately(t *testing.T) {
	w, h := newWaiter(t, waitCfg(0, 30*time.Minute))
	st := freshState()
	st.RunState = state.StateWaiting
	st.Usage.Waiting = true
	reset := h.now.Add(-time.Minute) // reset already passed
	st.Usage.ResetAt = &reset

	if err := w.Resume(context.Background(), st, h.statePath); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(h.slept) != 1 || h.slept[0] != 0 {
		t.Fatalf("past reset must resume immediately: slept=%v, want [0]", h.slept)
	}
	if st.RunState != state.StateRunning {
		t.Fatalf("run_state = %q, want RUNNING", st.RunState)
	}
}

func TestResumeBackoffWhenResetNil(t *testing.T) {
	w, h := newWaiter(t, waitCfg(0, 15*time.Minute))
	st := freshState()
	st.RunState = state.StateWaiting
	st.Usage.Waiting = true
	st.Usage.ResetAt = nil

	if err := w.Resume(context.Background(), st, h.statePath); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(h.slept) != 1 || h.slept[0] != 15*time.Minute {
		t.Fatalf("nil reset on resume must use backoff: slept=%v, want [15m]", h.slept)
	}
}

// --- contextSleep: the real interruptible sleep --------------------------

func TestContextSleepReturnsAfterDuration(t *testing.T) {
	start := time.Now()
	if err := contextSleep(context.Background(), 10*time.Millisecond); err != nil {
		t.Fatalf("contextSleep: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 5*time.Millisecond {
		t.Fatalf("returned too early: %v", elapsed)
	}
}

func TestContextSleepCancellable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := contextSleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled sleep err = %v, want context.Canceled", err)
	}
}

func TestContextSleepZeroDurationReturns(t *testing.T) {
	if err := contextSleep(context.Background(), 0); err != nil {
		t.Fatalf("zero sleep: %v", err)
	}
}
