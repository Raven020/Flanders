// Package usage is the subscription usage-limit wait/auto-resume mechanism —
// the guardrail that lets an unattended Ralph run survive a subscription window
// running out (specs/01-ralph-loop.md §Guardrails; specs/09-state-and-resume.md
// §Resume). It is what makes a multi-day run keep going: when the agent hits a
// rate-limit / usage-exhausted signal, the harness parses the reset time, parks
// the run in WAITING, sleeps until the window reopens, and resumes — no human in
// the loop.
//
// Division of labour. Detection is already done upstream: the stream package
// classifies a loop's outcome (stream.OutcomeUsageLimit) and extracts the window
// reset (LoopObservation.ResetAt). This package is the *reaction* to that
// classification — the decision (wait vs. halt, honoring [usage].on_limit and
// max_cycles) plus the wait mechanics (persist WAITING, sleep, resume). The
// orchestrator (Phase 5) owns the surrounding run loop; it calls HandleLimit when
// a loop returns OutcomeUsageLimit, and Resume on startup when it finds a
// WAITING state on disk.
//
// Why built before its consumer. The Phase-5 orchestrator that drives the loop
// does not exist yet. Mirroring src/lib/guardrail, src/lib/verify and
// src/lib/reconcile — all authored ahead of the orchestrator as standalone,
// fully-tested primitives — this package ships the complete wait/resume behavior
// now so the orchestrator only has to call it.
//
// Two correctness invariants the design pins down:
//
//   - Persist-before-sleep. HandleLimit writes WAITING + reset_at + the
//     incremented cycle counter to state.json *before* it sleeps. A crash during
//     the (possibly hours-long) wait must resume correctly on restart; the
//     orchestrator reads run_state==WAITING and calls Resume. If we slept first and
//     persisted after, a crash mid-wait would lose the fact that we were waiting.
//
//   - cycles_used counts windows *entered*, incremented at the start of each wait
//     (not on completion). This claims the window before sleeping, so a
//     crash-then-Resume cannot double-count it (Resume never bumps the counter).
//     The cap compares the count *before* the increment: with max_cycles=N the
//     harness drains exactly N windows then halts on the (N+1)th limit.
package usage

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"flanders/src/lib/config"
	"flanders/src/lib/state"
)

// Action is the decision taken when a usage limit is encountered.
type Action int

const (
	// ActionWait means the harness parks in WAITING, sleeps to the window reset,
	// and resumes. As a HandleLimit/Resume return it means "the wait completed;
	// the run should continue."
	ActionWait Action = iota
	// ActionHalt means the run stops — either [usage].on_limit=halt, or the
	// max_cycles cap has been reached.
	ActionHalt
)

func (a Action) String() string {
	switch a {
	case ActionWait:
		return "wait"
	case ActionHalt:
		return "halt"
	default:
		return "unknown"
	}
}

// Decision is the pure outcome of Decide: whether to wait or halt, and (when
// halting) the human-readable reason recorded in state.Halt.
type Decision struct {
	Action     Action
	HaltReason string // populated only when Action==ActionHalt
}

// Decide applies [usage].on_limit and the max_cycles cap to a freshly-detected
// usage limit. cyclesUsed is state.usage.cycles_used *before* this window — the
// number of windows already drained this run.
//
// Precedence: on_limit=halt wins outright (the operator chose to stop rather than
// sleep). Otherwise the cap: with max_cycles>0, once cyclesUsed has reached it the
// harness has drained its allowance and halts; max_cycles<=0 means unlimited
// (the documented default, drain windows forever). Anything but "halt" (including
// "wait" or an empty value) takes the wait path — the safe default that biases
// toward pausing rather than aborting an unattended run.
func Decide(cfg config.Usage, cyclesUsed int) Decision {
	if strings.EqualFold(strings.TrimSpace(cfg.OnLimit), "halt") {
		return Decision{Action: ActionHalt, HaltReason: "usage limit hit and [usage].on_limit=halt"}
	}
	if cfg.MaxCycles > 0 && cyclesUsed >= cfg.MaxCycles {
		return Decision{
			Action:     ActionHalt,
			HaltReason: fmt.Sprintf("usage limit: [usage].max_cycles=%d reached (cycles_used=%d)", cfg.MaxCycles, cyclesUsed),
		}
	}
	return Decision{Action: ActionWait}
}

// WaitDuration returns how long to sleep before resuming and whether the backoff
// fallback was used. resetAt is the parsed window reset (nil → none parsed):
//
//   - resetAt in the future → sleep exactly until then (the precise path).
//   - resetAt already in the past → 0 (resume immediately; spec 09 §resume).
//   - resetAt nil → fall back to [usage].backoff (clamped to >= 0).
//
// It is pure (now is passed in) so the decision is unit-testable without sleeping.
func WaitDuration(resetAt *time.Time, backoff time.Duration, now time.Time) (d time.Duration, usedBackoff bool) {
	if resetAt != nil {
		if rem := resetAt.Sub(now); rem > 0 {
			return rem, false
		}
		return 0, false // window already reset → resume immediately
	}
	if backoff < 0 {
		backoff = 0
	}
	return backoff, true
}

// Waiter performs the usage-limit wait/resume sequence against a *state.State,
// persisting every transition to statePath so a crash mid-wait resumes correctly.
// now/sleep are seams overridden in tests; production uses the real clock and an
// interruptible timer.
type Waiter struct {
	cfg config.Usage
	log *slog.Logger

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// NewWaiter builds a Waiter for the given [usage] config. A nil log uses the slog
// default.
func NewWaiter(cfg config.Usage, log *slog.Logger) *Waiter {
	if log == nil {
		log = slog.Default()
	}
	return &Waiter{
		cfg:   cfg,
		log:   log,
		now:   func() time.Time { return time.Now().UTC() },
		sleep: contextSleep,
	}
}

// HandleLimit reacts to a freshly-detected usage limit (a loop whose Outcome is
// stream.OutcomeUsageLimit). resetAt is the parsed window reset from the loop
// observation (nil → use [usage].backoff). It mutates st in place and persists to
// statePath at each transition, returning the Decision taken:
//
//   - ActionHalt — on_limit=halt or max_cycles reached: st is set HALTED with
//     st.Halt={reason, current_task}, persisted; the run stops.
//   - ActionWait — st.usage.cycles_used is incremented, st set WAITING with
//     usage.waiting=true and the parsed reset_at, and persisted *before* sleeping;
//     the harness then sleeps until the window resets, sets st RUNNING with
//     usage.waiting=false, and persists again. On return the run should continue.
//
// A cancelled ctx during the sleep returns ctx.Err() with st left WAITING on disk
// — a graceful shutdown; a later start re-enters via Resume. A Save failure is
// returned as an error (the harness cannot safely proceed if it cannot record that
// it is waiting).
func (w *Waiter) HandleLimit(ctx context.Context, st *state.State, statePath string, resetAt *time.Time) (Decision, error) {
	dec := Decide(w.cfg, st.Usage.CyclesUsed)
	if dec.Action == ActionHalt {
		st.RunState = state.StateHalted
		st.Usage.Waiting = false
		st.Halt = state.Halt{Reason: dec.HaltReason, Task: st.CurrentTask}
		if err := st.Save(statePath); err != nil {
			return dec, fmt.Errorf("usage: persist halt: %w", err)
		}
		w.log.Warn("usage limit: halting run", "reason", dec.HaltReason, "cycles_used", st.Usage.CyclesUsed)
		return dec, nil
	}

	// Wait path: claim this window (increment + persist) before sleeping so a crash
	// mid-wait resumes. Copy resetAt so st owns its own value, not the caller's.
	st.Usage.CyclesUsed++
	st.Usage.Waiting = true
	st.Usage.ResetAt = copyTime(resetAt)
	st.RunState = state.StateWaiting
	if err := st.Save(statePath); err != nil {
		return dec, fmt.Errorf("usage: persist wait: %w", err)
	}
	w.log.Info("usage limit: entering wait", "reset_at", st.Usage.ResetAt, "cycles_used", st.Usage.CyclesUsed)

	if err := w.sleepThenResume(ctx, st, statePath); err != nil {
		return dec, err
	}
	return dec, nil
}

// Resume re-enters a wait a prior process started: the orchestrator found
// st.RunState==WAITING on startup (spec 09 §resume). It sleeps the time remaining
// until st.Usage.ResetAt (0 if already past → resume immediately; [usage].backoff
// when reset_at is null), then flips st to RUNNING and persists. It does NOT touch
// cycles_used — that window was claimed when the wait first began (HandleLimit).
func (w *Waiter) Resume(ctx context.Context, st *state.State, statePath string) error {
	w.log.Info("usage wait: resuming prior wait", "reset_at", st.Usage.ResetAt, "cycles_used", st.Usage.CyclesUsed)
	return w.sleepThenResume(ctx, st, statePath)
}

// sleepThenResume sleeps until the window resets, then transitions st back to
// RUNNING and persists. Shared by HandleLimit's wait path and Resume. A cancelled
// ctx returns ctx.Err() with st left WAITING on disk — the persisted-before-sleep
// state from HandleLimit (or the prior process) is the resume cursor.
func (w *Waiter) sleepThenResume(ctx context.Context, st *state.State, statePath string) error {
	d, usedBackoff := WaitDuration(st.Usage.ResetAt, w.cfg.Backoff.Duration, w.now())
	w.log.Info("usage wait: sleeping", "duration", d, "used_backoff", usedBackoff)
	if err := w.sleep(ctx, d); err != nil {
		return err // cancelled → stays WAITING on disk; a restart resumes
	}
	st.RunState = state.StateRunning
	st.Usage.Waiting = false
	if err := st.Save(statePath); err != nil {
		return fmt.Errorf("usage: persist resume: %w", err)
	}
	w.log.Info("usage wait: window reset, resuming run")
	return nil
}

// copyTime returns a pointer to a copy of *t, or nil. Used so a persisted reset_at
// is owned by state and not aliased to the caller's observation.
func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

// contextSleep blocks for d or until ctx is cancelled, whichever comes first,
// returning ctx.Err() on cancellation. A non-positive d returns immediately (still
// honoring an already-cancelled ctx) so a past/zero reset resumes without delay.
func contextSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
