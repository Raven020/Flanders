// Package reconcile records the outcome of a loop onto the task file when the agent
// did not — the harness's half of the locked mutation-ownership split
// (specs/02-plan-and-tasks.md §Mutation ownership). The agent owns flipping its
// task's `status` (`active` while working, `done`/`blocked` on exit); the harness
// "never depends solely on the agent remembering." So after every loop the harness
// reconciles against signals it owns:
//
//   - the agent flipped to a terminal status (done/blocked) → respect it verbatim;
//   - the agent left it non-terminal but the ground-truth test gate passed → the
//     work is in fact complete, so write `done`;
//   - the agent left it stuck at `active` → reset to `pending` so the selector
//     re-picks it (the selector only returns `pending`; `active` is a mid-loop
//     state, not a resting one);
//   - otherwise leave it `pending` for a retry.
//
// Precedence is agent-status-first, then inference (spec 01 §reconciliation order):
// an explicit terminal flip is never second-guessed here. The run-level done
// decision — test gate ∧ every task done ∧ no stall (task 3.7) — is where a false
// `done` is caught, not in per-task reconciliation.
//
// Why a separate package (not folded into loop): the same reconciliation runs in
// two places — at the end of each build loop (the loop driver) and on resume of a
// crashed RUNNING state, re-deriving status from ground truth before continuing
// (spec 09 §resume, the orchestrator). Isolating the decision keeps one source of
// truth for "what status does this loop's evidence imply" and makes it testable
// without a live loop. It imports only task: the test-gate verdict arrives as plain
// booleans, so reconcile depends on neither verify nor git (the git "did work
// happen" signal feeds the stall guardrail and the journal, recorded by the caller —
// it does not, on its own, prove a task done, so it is not a reconcile input).
package reconcile

import (
	"fmt"

	"flanders/src/lib/task"
)

// Action names what the harness decided about a task's status after a loop. It is
// surfaced for the journal/TUI and the orchestrator's logging; the resulting status
// is in [Result.To].
type Action string

const (
	// ActionRespected: the agent flipped its status to a terminal value (done or
	// blocked); the harness keeps it verbatim — the agent owns its status (spec 02,
	// option A), and the run-level done-gate (task 3.7) catches a false done.
	ActionRespected Action = "respected"
	// ActionPromoted: the agent left a non-terminal status, but the ground-truth test
	// gate passed, so the work is complete — the harness writes `done`. This is the
	// "never depend on the agent remembering" fallback.
	ActionPromoted Action = "promoted"
	// ActionNormalized: the agent left the task `active` (it set active while working
	// but never flipped on exit — e.g. a timed-out or killed loop). `active` is not a
	// resting state and the selector only re-picks `pending`, so the harness resets it
	// to `pending` to keep it selectable.
	ActionNormalized Action = "normalized"
	// ActionUnchanged: the agent left the task `pending` and nothing proved it done; it
	// stays pending for a retry. Genuine no-progress is caught by the stall guardrail
	// (task 3.9, which reads the git "work happened" signal) — reconcile never invents
	// a block, because the blocked taxonomy needs a reason it cannot infer.
	ActionUnchanged Action = "unchanged"
)

// Signals carries the ground-truth verdict reconcile decides from. Only the test
// gate informs the status: it is the one signal that can prove a task done
// (spec 01 §done-detection). TestRan distinguishes "the gate did not run this loop"
// (a non-code phase, or a non-clean invocation) from "ran and failed"; reconcile
// promotes to done only when the gate both ran and passed.
type Signals struct {
	TestRan    bool // the harness test gate ran this loop
	TestPassed bool // ... and the canonical test command exited 0 (ground-truth done)
}

// Result is what reconciliation decided and did. Wrote reports whether the task
// file was rewritten (true exactly when To differs from From), so the caller can
// log/checkpoint precisely without re-reading the file.
type Result struct {
	Action Action
	From   task.Status // status the agent left when the loop ended
	To     task.Status // status after reconciliation
	Reason task.Reason // set iff To == blocked (carried from the agent's block)
	Wrote  bool        // the harness rewrote the task file
}

// Reconcile applies the policy in the package doc to t (the task as it stands on
// disk after the loop, with any agent edits) and writes the file when it changes
// the status. It is idempotent on a settled task: a done/blocked task is respected
// and never rewritten. A write error is returned (the harness must know it failed
// to record the outcome); on success the in-memory t is left mutated to match.
func Reconcile(t *task.Task, sig Signals) (Result, error) {
	from := t.Status()
	res := Result{From: from, To: from, Reason: t.Reason()}

	// 1. Agent-written terminal status wins (precedence: agent first).
	if from == task.StatusDone || from == task.StatusBlocked {
		res.Action = ActionRespected
		return res, nil
	}

	// 2. Inference fallback — the agent left a non-terminal status. The test gate is
	// the only signal that can prove the work complete; if it ran and passed, the
	// harness records the done the agent forgot to flip.
	if sig.TestRan && sig.TestPassed {
		t.SetStatus(task.StatusDone) // SetStatus also clears any stale reason
		if err := t.WriteFile(""); err != nil {
			return Result{}, fmt.Errorf("reconcile: write promoted status for task %s: %w", t.ID(), err)
		}
		res.Action = ActionPromoted
		res.To, res.Reason, res.Wrote = task.StatusDone, "", true
		return res, nil
	}

	// 3. Not provably done. Normalize a stuck `active` back to `pending`; a `pending`
	// task is already selectable, so it is left untouched (no needless write).
	if from == task.StatusActive {
		t.SetStatus(task.StatusPending)
		if err := t.WriteFile(""); err != nil {
			return Result{}, fmt.Errorf("reconcile: normalize active→pending for task %s: %w", t.ID(), err)
		}
		res.Action = ActionNormalized
		res.To, res.Wrote = task.StatusPending, true
		return res, nil
	}

	res.Action = ActionUnchanged
	return res, nil
}
