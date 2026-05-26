package loop

import (
	"context"

	"flanders/src/lib/guardrail"
	"flanders/src/lib/stream"
	"flanders/src/lib/task"
)

// The per-task build flow (plan task 4.5; specs/06-orchestration.md §Phases, spec 07
// §Per-task flow within the build phase). Inside the build phase EVERY task runs
// test → build → verify (TDD, always-on):
//
//	for the next actionable task:
//	  1. test loop   — the test agent ensures a red acceptance test (or finds one):
//	                     ├ already satisfied (green) → mark done, SKIP build
//	                     ├ a red acceptance test is ready → proceed to build
//	                     └ no red test produced → re-run the test loop (bounded)
//	  2. build loop(s)— the build agent implements until the harness test gate passes
//	  3. verify       — the harness-owned test command exit code is the ground truth
//
// [Driver.RunTask] is that flow for ONE task: it composes [Driver.Iterate] calls (the
// single-loop spine) into the test→build sequence, reading [Result.TestVerdict] (plan
// task 4.4) to route, and drives the task to a terminal [Disposition] the orchestrator
// acts on without re-deriving anything.
//
// Why RunTask runs multiple loops itself, and what stays the orchestrator's. RunTask is
// the SMALLEST orchestration unit above the single-loop driver — it owns the test→build
// sequencing and the "keep building this task until it is done/blocked" inner loop,
// because those decisions are local to one task. The run-state machine above it (Phase 5)
// stays the orchestrator's: it owns persisting state.json, the phase budget across tasks,
// the usage-limit wait/resume, and the drain→batch-replan phase transitions (spec 06). The
// two coupling points are passed through explicitly rather than duplicated: the build-phase
// iteration count `iter` (so RunTask honors the per-phase [guardrails].max_iterations cap
// across its inner loops, and feeds the checkpoint {iter} variable) and the run-level
// consecutive-no-progress `stallCount` (threaded through every inner loop via the
// guardrail primitives, so a no-progress task cannot burn the whole phase budget before
// the stall guardrail fires — then returned for the orchestrator to persist). RunTask
// itself never touches state.json.
//
// Why test loops count toward the same budget as build loops. Spec 06 frames the per-task
// cycle as test → build → verify all WITHIN the build phase, so RunTask counts both kinds
// of Iterate against `iter`/max_iterations and threads stall across both. (The finer
// plan-vs-build budget apportionment is spec-06 OPEN and the orchestrator's; within the
// build phase the test sub-step shares the build budget.)
//
// Ground-truth, not agent self-report. The disposition is derived from the harness's own
// signals — [Result.TestVerdict] (itself the test agent's status flip corroborated by the
// whole-suite gate), [Result.Reconcile] (the post-loop status the harness recorded), the
// git work-happened signal, and the loop outcome — never from the agent merely claiming
// success. A build loop that ends with the task `done` carries [TaskFlowResult.Verified]
// (the final gate passed) so a `done` the agent flipped without a green gate is visible to
// the orchestrator's run-level done-detection (spec 01 §done-detection), which is where a
// false `done` is caught (spec 02 §Mutation ownership), not here.

// Disposition is the terminal outcome of one [Driver.RunTask] — what happened to the task
// and what the orchestrator should do next. Every value is a real, recordable outcome the
// spec enumerates (or a degraded case the guardrails bound); the full per-loop detail is on
// [TaskFlowResult.Results]/[TaskFlowResult.Last].
type Disposition string

const (
	// DispNoWork: no actionable task this flow — the build drain is exhausted. Read
	// [TaskFlowResult.AllDone] to tell "every task done" (success) from "stalled"
	// (everything left blocked/active, the batch-replan trigger). No agent ran.
	DispNoWork Disposition = "no-work"
	// DispDone: the task reached `done`. [TaskFlowResult.Verified] reports whether the
	// harness gate corroborated it (the red→green→verified happy path) or the agent flipped
	// done without a green gate (the orchestrator's run-level gate is the backstop).
	DispDone Disposition = "done"
	// DispBlocked: the task reached `blocked`. [TaskFlowResult.Reason] carries the taxonomy
	// the orchestrator routes on — context-overreach → fresh split (plan task 4.6),
	// new-scope/dependency/error → defer to the batched re-plan (spec 06 §Refinement).
	DispBlocked Disposition = "blocked"
	// DispNoRedTest: the test agent never produced a red acceptance test and never confirmed
	// satisfaction, across the bounded retries — a degraded case spec 07 does not enumerate.
	// The orchestrator decides (surface, or defer to re-plan); RunTask refuses to build
	// against no red test (the TDD invariant).
	DispNoRedTest Disposition = "no-red-test"
	// DispStalled: N consecutive no-progress loops (no file change AND no status change) —
	// the classic Ralph failure mode (spec 01 §Guardrails). The orchestrator halts + surfaces.
	DispStalled Disposition = "stalled"
	// DispMaxIterations: the per-phase iteration cap was reached before the task finished
	// (spec 01 §Guardrails). The orchestrator halts + surfaces.
	DispMaxIterations Disposition = "max-iterations"
	// DispUsageLimit: a loop hit the subscription usage/rate limit. The orchestrator waits
	// until reset and resumes (plan task 3.12, src/lib/usage), then re-enters the flow.
	DispUsageLimit Disposition = "usage-limit"
	// DispTimeout: a loop exceeded [guardrails].iteration_timeout and was killed. Reported
	// distinctly from DispError so the orchestrator can record/retry it as it chooses.
	DispTimeout Disposition = "timeout"
	// DispError: a loop ended in a genuine agent/API/process error. The orchestrator's
	// error path (retry/halt) handles it.
	DispError Disposition = "error"
)

// defaultMaxTestAttempts bounds how many times RunTask re-runs the test loop on the
// degraded DispNoRedTest case (the test agent left no red test and claimed no satisfaction).
// Small on purpose: one re-attempt absorbs a transient miss, but a test agent that
// persistently produces nothing is a real problem to surface, not to retry indefinitely.
// The stall guardrail and the phase iteration cap are the harder backstops above this.
const defaultMaxTestAttempts = 2

// TaskFlowResult is everything the orchestrator needs after driving one task — the
// disposition to route on, the run-level counters to persist, and the per-loop history.
type TaskFlowResult struct {
	// Disposition is the terminal outcome (see the Disp* constants).
	Disposition Disposition
	// Task is the task this flow drove, or nil for DispNoWork.
	Task *task.Task
	// AllDone is meaningful only for DispNoWork: true when every task is `done` (the run
	// success signal), false when work remains but nothing is actionable (a stall/drain).
	AllDone bool
	// Reason is set for DispBlocked: the blocked taxonomy the orchestrator routes on.
	Reason task.Reason
	// TestVerdict is the test phase's routing decision for this task (TestVerdictNone if the
	// test loop did not complete cleanly).
	TestVerdict TestVerdict
	// Verified reports whether the final loop's harness test gate passed — the ground-truth
	// "this task is green" signal. True only on a gate-corroborated DispDone.
	Verified bool

	// Iterations is the number of agent-spawning loops this flow ran (test + build). The
	// orchestrator adds it to state.iter.build/total. A DispNoWork loop spawns nothing and
	// is NOT counted.
	Iterations int
	// StallCount is the run-level consecutive-no-progress counter after this flow, threaded
	// from the stallCount argument through every inner loop. The orchestrator persists it to
	// state.stall.count.
	StallCount int

	// Results is each spawning loop's Result in order; Last is the final one (== the last
	// element of Results, or the NoWork result). Both nil only if no loop ran at all.
	Results []*Result
	Last    *Result
}

// RunTask drives the next actionable task through the TDD test → build → verify cycle and
// returns its terminal [Disposition] (spec 06/07; plan task 4.5). It selects no task itself
// — each inner [Driver.Iterate] hot-reloads the store and picks the next actionable task,
// which stays the same task across the test and build loops because a red-but-unbuilt task
// remains the lowest-id actionable one until it is built (done) or blocked.
//
// iter is the orchestrator's current build-phase iteration count; RunTask passes iter +
// (loops run so far) to each Iterate and uses it with [guardrails].max_iterations to stop
// at the per-phase cap. stallCount is the run-level consecutive-no-progress counter coming
// in; it is threaded through the inner loops and returned on [TaskFlowResult.StallCount].
//
// A returned error is an infrastructure failure from an inner Iterate (couldn't read the
// plan, build the command, or spawn) — the orchestrator surfaces and halts. A loop that ran
// but produced a usage-limit/error/timeout RESULT is not an error: it ends the flow with the
// matching disposition (DispUsageLimit/DispError/DispTimeout), the same contract as Iterate.
func (d *Driver) RunTask(ctx context.Context, iter, stallCount int) (*TaskFlowResult, error) {
	maxIters := d.cfg.Guardrails.MaxIterations
	stallN := d.cfg.Guardrails.StallN

	out := &TaskFlowResult{StallCount: stallCount}

	// ---- TEST PHASE: ensure a red acceptance test exists for the task (spec 07 §test
	// agent). Bounded retry on the degraded "no red test produced" case; the other verdicts
	// terminate the flow (satisfied → skip build; blocked → drain) or break out to build.
	redReady := false
	for attempt := 0; attempt < defaultMaxTestAttempts; attempt++ {
		if guardrail.MaxIterationsReached(iter+out.Iterations, maxIters) {
			out.Disposition = DispMaxIterations
			return out, nil
		}
		res, err := d.Iterate(ctx, "test", iter+out.Iterations)
		if err != nil {
			return nil, err
		}
		if res.NoWork {
			out.Last = res
			out.Disposition = DispNoWork
			out.AllDone = res.AllDone
			return out, nil
		}
		out.record(res)
		out.Task = res.Task
		out.TestVerdict = res.TestVerdict
		out.StallCount = guardrail.StallStep(out.StallCount, progressed(res))

		if res.Outcome != stream.OutcomeSuccess {
			out.Disposition = dispositionForOutcome(res)
			return out, nil
		}

		switch res.TestVerdict {
		case TestVerdictSatisfied:
			// Branch 2: the acceptance is already met (agent done + green gate) — skip build.
			out.Disposition = DispDone
			out.Verified = res.Verify != nil && res.Verify.Passed()
			return out, nil
		case TestVerdictBlocked:
			out.Disposition = DispBlocked
			out.Reason = res.Reconcile.Reason
			return out, nil
		case TestVerdictRedReady:
			// Branches 1/3: a red acceptance test now exists — build it.
			redReady = true
		default: // TestVerdictNoRedTest (or None defensively): re-run the test loop unless
			// we have stalled. Continue here; the loop bound + the max-iter check cap retries.
			if guardrail.StallTripped(out.StallCount, stallN) {
				out.Disposition = DispStalled
				return out, nil
			}
		}
		if redReady {
			break
		}
	}
	if !redReady {
		// Retries exhausted without a red test. Refuse to build against nothing (TDD
		// invariant) and surface the degraded case for the orchestrator to decide.
		out.Disposition = DispNoRedTest
		return out, nil
	}

	// ---- BUILD PHASE: implement until the harness test gate passes (spec 07 step 2/3). The
	// same task is selected each loop until it reaches a terminal status; verify is the gate
	// run inside Iterate, surfaced on Result.Verify/Reconcile.
	for {
		if guardrail.MaxIterationsReached(iter+out.Iterations, maxIters) {
			out.Disposition = DispMaxIterations
			return out, nil
		}
		res, err := d.Iterate(ctx, "build", iter+out.Iterations)
		if err != nil {
			return nil, err
		}
		if res.NoWork {
			// The task left the actionable set without this loop seeing it terminal (e.g. an
			// out-of-band change). Surface as drain so the orchestrator re-evaluates the plan.
			out.Last = res
			out.Disposition = DispNoWork
			out.AllDone = res.AllDone
			return out, nil
		}
		out.record(res)
		out.Task = res.Task
		out.StallCount = guardrail.StallStep(out.StallCount, progressed(res))

		if res.Outcome != stream.OutcomeSuccess {
			out.Disposition = dispositionForOutcome(res)
			return out, nil
		}

		switch res.Reconcile.To {
		case task.StatusDone:
			out.Disposition = DispDone
			out.Verified = res.Verify != nil && res.Verify.Passed()
			return out, nil
		case task.StatusBlocked:
			out.Disposition = DispBlocked
			out.Reason = res.Reconcile.Reason
			return out, nil
		}

		// Task is still non-terminal → another build loop, unless we have stalled. (The
		// per-phase max-iter cap is re-checked at the top of the next iteration.)
		if guardrail.StallTripped(out.StallCount, stallN) {
			out.Disposition = DispStalled
			return out, nil
		}
	}
}

// record counts one agent-spawning loop and appends it to the history. A DispNoWork loop
// spawns nothing and is deliberately not recorded here (the caller handles it).
func (r *TaskFlowResult) record(res *Result) {
	r.Iterations++
	r.Results = append(r.Results, res)
	r.Last = res
}

// progressed reports whether a loop made progress for stall accounting (spec 01 §Stall: a
// stalled loop has "no file changes AND no task-status change"). It uses the same
// guardrail.Changed rule the orchestrator would, fed from the loop Result's git
// work-happened signal and the reconcile status transition — so RunTask's inner stall
// threading and the orchestrator's are one rule, not two.
func progressed(res *Result) bool {
	statusChanged := res.Reconcile.From != res.Reconcile.To
	return guardrail.Changed(res.WorkHappened, statusChanged)
}

// dispositionForOutcome maps a non-success loop outcome to the flow disposition. A timeout
// is reported distinctly even though Classify folds a killed process into OutcomeError: the
// supervisor sets Result.TimedOut when the kill was the iteration-timeout deadline, and the
// orchestrator may record/retry a timeout differently from a genuine error.
func dispositionForOutcome(res *Result) Disposition {
	if res.TimedOut {
		return DispTimeout
	}
	switch res.Outcome {
	case stream.OutcomeUsageLimit:
		return DispUsageLimit
	default:
		return DispError
	}
}
