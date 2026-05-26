// Package guardrail holds the run-level "evaluate" decisions of the Ralph loop —
// step 6 of the iteration anatomy, "update task status; check guardrails; decide
// loop/stop" (specs/01-ralph-loop.md §Iteration anatomy). It is the single source
// of truth for three decisions the orchestrator makes after each loop:
//
//   - done-detection (§Done-detection, locked) — is the run/phase complete?
//   - stall (§Guardrails, "Stall detection") — have N consecutive loops made no
//     progress, the classic Ralph failure mode?
//   - max-iterations (§Guardrails, "Max iterations") — has a phase hit its cap?
//
// Why a pure package, and why separate from loop/state. These are policy, not
// persistence: each function is a pure predicate over plain ints/bools, so the
// counters they read (state.stall.count, state.iter.*) and the limits they compare
// against (guardrails.stall_n, guardrails.max_iterations) stay owned by the state
// cache and the config, while the *rule* lives in one tested place. The package
// imports nothing — the test verdict, the work-happened signal, and the "all tasks
// done" flag all arrive as plain booleans the caller already has on loop.Result and
// task.Store, so guardrail couples to neither verify, git, task, nor state.
//
// Why this exists before its consumer. The orchestrator (Phase 5) owns the
// run-state machine — incrementing state.iter, persisting state.stall.count, and
// transitioning run_state to HALTED/DONE — and is what composes these predicates
// into a verdict. That machine is not built yet, so (mirroring src/lib/reconcile
// and src/lib/verify, both authored ahead of the orchestrator) this package ships
// the decisions as standalone, fully-tested primitives. Deliberately NOT here: the
// precedence among a simultaneous done/halt, the choice of which iter counter
// (plan vs build) to check, and writing run_state/halt to disk — all the
// orchestrator's, which has the full run context to order them (spec 06).
package guardrail

// Changed reports whether a loop made progress for stall accounting: it touched
// files OR changed a task's status. Spec 01 defines a stalled loop as one with "no
// file changes AND no task-status change", so a loop counts as progress when
// *either* happened (the negation of that conjunction). The caller derives the two
// inputs from one loop's outcome: filesChanged = loop.Result.WorkHappened, and
// statusChanged = the reconcile decision moved the status (Result.Reconcile.From !=
// Result.Reconcile.To).
func Changed(filesChanged, statusChanged bool) bool {
	return filesChanged || statusChanged
}

// StallStep returns state.stall.count after one loop: reset to 0 when the loop made
// progress, else prev+1. This is the counter's transition (spec 01 §Stall: "N
// consecutive loops produce no file changes AND no task-status change"); the
// orchestrator reads the prior count off state.json, calls this, and persists the
// result. A single progress loop clears the streak — the counter measures
// *consecutive* no-change loops, not a lifetime total.
func StallStep(prev int, changed bool) int {
	if changed {
		return 0
	}
	return prev + 1
}

// StallTripped reports whether the stall guardrail has tripped: count consecutive
// no-change loops has reached the configured limit n (guardrails.stall_n, default
// 3). With n=3 the trip fires on the 3rd consecutive no-change loop (count==3),
// since StallStep counts up from 0. n<=0 disables the guardrail — config validates
// stall_n>=1, so that branch is defensive against a hand-edited state/config.
func StallTripped(count, n int) bool {
	return n > 0 && count >= n
}

// MaxIterationsReached reports whether a phase has hit its hard iteration cap
// (guardrails.max_iterations, default 100, scoped "per phase" by spec 01). iter is
// the number of loops run in the *current* phase — the orchestrator passes
// state.iter.plan or state.iter.build depending on the active phase (the per-phase
// vs global apportionment of a single max_iterations is spec-06 OPEN and the
// orchestrator's to resolve; this predicate only compares the counter it is given).
// max<=0 disables the cap. The comparison is >= so the cap is inclusive: the run
// halts once the count of completed loops reaches max.
func MaxIterationsReached(iter, max int) bool {
	return max > 0 && iter >= max
}

// Done reports whether the run/phase is complete. This is the locked done-detection
// rule (spec 01 §Done-detection, harness-owned): done iff ALL of
//
//  1. testPassed — the canonical test command exited 0. This is harness-owned
//     ground truth (verify.Result.Passed()), NOT the agent's self-report.
//  2. allTasksDone — every task in the plan is `done` (task.Store.AllDone()); none
//     left pending or blocked.
//  3. !stalled — no stall is in effect (the negation of StallTripped).
//
// The agent may report completion, but that report is advisory and is deliberately
// not a parameter here — the stop condition is the harness's alone (spec 01). A run
// that has tripped a halt guardrail (stall, max-iterations) is not "done"; the
// orchestrator surfaces those as HALTED before it ever asks whether the run is done.
func Done(testPassed, allTasksDone, stalled bool) bool {
	return testPassed && allTasksDone && !stalled
}
