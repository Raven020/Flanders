package loop

import "flanders/src/lib/task"

// The TDD test phase (plan task 4.4; specs/07-agents-and-models.md §The `test` agent;
// specs/06-orchestration.md §test-build-verify). Within the build phase every task runs
// test → build → verify: a `test` agent — a DIFFERENT agent than the implementer, so the
// build agent cannot weaken its own success criterion, which is what makes the harness's
// ground-truth gate trustworthy — ensures a FAILING acceptance test exists for the task
// before any implementation happens. The test agent always CHECKS and only WRITES when
// needed, in one of three branches (spec 07):
//
//  1. a red acceptance test already exists → reuse it, write nothing → proceed to build;
//  2. a test already PASSES for the acceptance → the behaviour is already implemented, so
//     the harness marks the task `done` and SKIPS the build loop;
//  3. no test → the agent writes the smallest test that encodes the acceptance and fails.
//
// Why this rides the Iterate spine, not a separate method like PlanIterate. A test loop is
// task-selected, composes around one task, runs the harness test gate, and reconciles one
// task's status — the whole of Iterate. Only two things differ, both localized in Iterate:
// the composed prompt uses the test-agent role (compose.go taskRoleHeader) instead of the
// build role, and the post-loop evaluation derives a [TestVerdict] rather than letting a
// green gate auto-promote the task to done. The plan loop, by contrast, shares none of the
// selected-task spine, which is why it (and only it) is a separate driver method.
//
// Resolving the spec-07 OPEN ("how the test agent locates an existing test for this
// acceptance"). The harness determines per-task red/green by combining the signal each
// side owns: the test agent's status flip (it alone knows which test covers the acceptance
// — branch 2 is an explicit `done`) and the harness's whole-suite test gate (the
// ground-truth red/green signal, src/lib/verify). In the per-task test→build→verify flow
// (plan task 4.5) the suite is green before each test loop — every prior task is done and
// passing — so a freshly RED suite after the test loop reliably means THIS task's
// acceptance test is red, and a still-GREEN suite means the agent wrote no red test (or
// confirmed satisfaction). A finer per-task filtered gate (run only this task's test) is a
// documented future refinement; the whole-suite gate is the v1 floor, consistent with
// verify running the whole [commands].test. Crucially the harness never SKIPS build on an
// unconfirmed `done` (agent says satisfied but the suite is not green): it conservatively
// proceeds to build, where green is established for real (spec 00 decision 2 — ground
// truth, not agent self-report).

// TestVerdict is the test phase's per-task routing decision (plan task 4.4), surfaced on
// [Result.TestVerdict] for the per-task build flow (4.5) and the orchestrator. It is the
// zero value [TestVerdictNone] for any non-test loop (build/plan) and for a test loop that
// did not complete cleanly (a usage limit / error / timeout — the orchestrator routes
// those on [Result.Outcome], not on a verdict).
type TestVerdict string

const (
	// TestVerdictNone: not a test loop, or the test loop did not complete cleanly.
	TestVerdictNone TestVerdict = ""
	// TestVerdictSatisfied: the acceptance is already met — the test agent reported the
	// task `done` and the harness gate corroborates it (a green suite). The build loop is
	// SKIPPED and the task stays `done` (spec 07 branch 2).
	TestVerdictSatisfied TestVerdict = "satisfied"
	// TestVerdictRedReady: a failing acceptance test now exists (reused per branch 1, or
	// freshly written per branch 3) — proceed to the build loop, which makes it pass.
	TestVerdictRedReady TestVerdict = "red-ready"
	// TestVerdictNoRedTest: the test agent left no failing test and did not confirm the
	// acceptance is satisfied (a green suite with the task still non-terminal). This is a
	// degraded outcome the spec does not enumerate; the orchestrator re-runs the test loop
	// (bounded by the max-iterations guardrail) rather than build against no red test.
	TestVerdictNoRedTest TestVerdict = "no-red-test"
	// TestVerdictBlocked: the test agent blocked the task (e.g. the acceptance cannot be
	// tested without a missing dependency — new-scope/dependency). The orchestrator routes
	// it through the drain/batch-replan path (spec 06), not the build loop.
	TestVerdictBlocked TestVerdict = "blocked"
)

// classifyTestVerdict maps the task's post-reconcile status and the ground-truth gate
// result to the test phase's routing decision (see the package-level test-phase doc). It
// is pure so the branch table is unit-testable without a live loop; the one status
// side-effect it implies — demoting an unconfirmed `done` (RedReady) to pending so the
// build loop can select it — is applied by the caller in Iterate.
func classifyTestVerdict(status task.Status, gateRan, gatePassed bool) TestVerdict {
	switch status {
	case task.StatusBlocked:
		return TestVerdictBlocked
	case task.StatusDone:
		// Branch 2: the agent reports the acceptance already satisfied. Corroborate with the
		// gate — a green suite confirms it; a suite that RAN and FAILED cannot confirm this
		// task in isolation (the whole-suite gate is coarse), so the harness does not skip
		// build (it proceeds and establishes green there). A gate that did not run leaves the
		// agent's word as the only signal → satisfied (the test command is required for the
		// build/test phases, so this is the no-test-command edge only).
		if gateRan && !gatePassed {
			return TestVerdictRedReady
		}
		return TestVerdictSatisfied
	default:
		// Non-terminal (reconcile normalized active→pending). A failing suite means a red
		// acceptance test exists → build it (branches 1/3). A green suite means no red test
		// was produced and satisfaction was not claimed → degraded; re-run the test loop.
		if gateRan && !gatePassed {
			return TestVerdictRedReady
		}
		return TestVerdictNoRedTest
	}
}
