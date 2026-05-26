package orchestrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flanders/src/lib/config"
	"flanders/src/lib/loop"
	"flanders/src/lib/paths"
	"flanders/src/lib/state"
	"flanders/src/lib/stream"
	"flanders/src/lib/task"
	"flanders/src/lib/usage"
	"flanders/src/lib/verify"
)

// These tests exercise the orchestrator's phase machine (spec 06) against a scripted
// fake driver that performs the SAME on-disk task-file mutations the real
// agent+reconcile would — so the orchestrator reads genuine ground truth from disk
// for its drain/all-done/blocked decisions, while the test controls the loop
// outcomes without a live `claude`. The loop package's own tests cover
// Iterate/RunTask/PlanIterate end-to-end against a stub process; here the unit under
// test is the run-state machine: plan→build, drain-then-batch-replan, done-detection,
// the guardrail halts, usage wait/resume, and crash resume.

// ---- scriptable fake driver -------------------------------------------------------

type fakeDriver struct {
	tasksDir string

	// planComplete answers the entry-point completeness check (planToComplete skips the
	// plan phase when it is already complete). Defaults to "complete".
	planComplete func() (*loop.Coverage, error)
	// planIterate runs one plan loop. The default returns a complete plan having done
	// nothing; tests override it to seed/resolve task files.
	planIterate func(f *fakeDriver, iter int) (*loop.Result, error)

	// buildOutcome[id] is the terminal status driveNext drives task id to (done by
	// default); buildReason[id] is the block reason when blocked.
	buildOutcome map[string]task.Status
	buildReason  map[string]task.Reason
	// runTaskOverride, when set, replaces the realistic driveNext drain (used to inject
	// stall/max-iter/usage-limit dispositions directly).
	runTaskOverride func(f *fakeDriver, iter, stall int) (*loop.TaskFlowResult, error)

	planN int // PlanIterate call count
	runN  int // RunTask call count
}

func (f *fakeDriver) PlanComplete() (*loop.Coverage, error) {
	if f.planComplete != nil {
		return f.planComplete()
	}
	return &loop.Coverage{}, nil
}

func (f *fakeDriver) PlanIterate(_ context.Context, iter int) (*loop.Result, error) {
	f.planN++
	if f.planIterate != nil {
		return f.planIterate(f, iter)
	}
	return planRes(true), nil
}

func (f *fakeDriver) RunTask(_ context.Context, iter, stall int) (*loop.TaskFlowResult, error) {
	f.runN++
	if f.runTaskOverride != nil {
		return f.runTaskOverride(f, iter, stall)
	}
	return f.driveNext(stall)
}

// driveNext mirrors the real RunTask: it selects the next actionable task from disk
// and drives it to its scripted terminal status (writing the file), or returns
// DispNoWork when the drain is exhausted.
func (f *fakeDriver) driveNext(stall int) (*loop.TaskFlowResult, error) {
	store, err := task.LoadDir(f.tasksDir)
	if err != nil {
		return nil, err
	}
	next, err := store.Next()
	if err != nil {
		return nil, err
	}
	if next == nil {
		return &loop.TaskFlowResult{Disposition: loop.DispNoWork, AllDone: store.AllDone(), StallCount: stall}, nil
	}
	id := next.ID()
	res := &loop.Result{SessionID: "b-" + id, Observation: &stream.LoopObservation{Cost: 0.02}, Outcome: stream.OutcomeSuccess}
	tfr := &loop.TaskFlowResult{Task: next, Iterations: 1, StallCount: stall, Results: []*loop.Result{res}, Last: res}

	if f.buildOutcome[id] == task.StatusBlocked {
		next.SetBlocked(f.buildReason[id])
		if err := next.WriteFile(""); err != nil {
			return nil, err
		}
		tfr.Disposition = loop.DispBlocked
		tfr.Reason = f.buildReason[id]
		return tfr, nil
	}
	next.SetStatus(task.StatusDone)
	if err := next.WriteFile(""); err != nil {
		return nil, err
	}
	tfr.Disposition = loop.DispDone
	tfr.Verified = true
	return tfr, nil
}

// seedPending writes fresh pending task files.
func (f *fakeDriver) seedPending(ids ...string) error {
	for _, id := range ids {
		if err := writeTaskFile(f.tasksDir, id, task.StatusPending, ""); err != nil {
			return err
		}
	}
	return nil
}

// resolveToPending flips blocked task files back to pending (the re-plan agent
// resolving a block) and marks them to build to done next drain.
func (f *fakeDriver) resolveToPending(ids ...string) error {
	for _, id := range ids {
		p := filepath.Join(f.tasksDir, id+"-task.md")
		tk, err := task.ParseFile(p)
		if err != nil {
			return err
		}
		tk.SetStatus(task.StatusPending)
		if err := tk.WriteFile(""); err != nil {
			return err
		}
		f.buildOutcome[id] = task.StatusDone
	}
	return nil
}

func planRes(complete bool) *loop.Result {
	r := &loop.Result{
		Phase:        "plan",
		SessionID:    "p",
		Observation:  &stream.LoopObservation{Cost: 0.01},
		Outcome:      stream.OutcomeSuccess,
		WorkHappened: true,
	}
	if complete {
		r.PlanComplete = &loop.Coverage{}
	} else {
		r.PlanComplete = &loop.Coverage{Uncovered: []loop.Requirement{{Spec: "x.md", Section: "y"}}}
	}
	return r
}

func incompleteCoverage() (*loop.Coverage, error) {
	return &loop.Coverage{Uncovered: []loop.Requirement{{Spec: "x.md", Section: "y"}}}, nil
}

// ---- helpers ----------------------------------------------------------------------

func writeTaskFile(dir, id string, status task.Status, reason task.Reason) error {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: \"%s\"\n", id)
	fmt.Fprintf(&b, "status: %s\n", status)
	if reason != "" {
		fmt.Fprintf(&b, "reason: %s\n", reason)
	}
	b.WriteString("acceptance: it works\n")
	b.WriteString("---\n\nbody for task " + id + "\n")
	return os.WriteFile(filepath.Join(dir, id+"-task.md"), []byte(b.String()), 0o644)
}

func setup(t *testing.T, fd *fakeDriver) *Orchestrator {
	t.Helper()
	if fd.buildOutcome == nil {
		fd.buildOutcome = map[string]task.Status{}
	}
	if fd.buildReason == nil {
		fd.buildReason = map[string]task.Reason{}
	}
	root := t.TempDir()
	p, err := paths.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureFlanders(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.Tasks, 0o755); err != nil {
		t.Fatal(err)
	}
	fd.tasksDir = p.Tasks

	cfg := config.Default()
	cfg.Commands.Test = "true" // satisfy ValidateForBuild; the gate itself is stubbed
	st := state.New(state.PhaseOrchestrate)

	o, err := New(Options{Driver: fd, Config: &cfg, Paths: p, State: st, StatePath: p.State})
	if err != nil {
		t.Fatal(err)
	}
	o.now = func() time.Time { return time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC) }
	o.st.StartedAt = o.now()
	// Default done-gate: green. Tests that probe the false-done backstop override it.
	o.verify = func(_ context.Context, _ string, _ config.Commands) verify.Result {
		return verify.Result{Test: verify.CommandResult{Ran: true, ExitCode: 0}}
	}
	return o
}

func run(t *testing.T, o *Orchestrator) *Summary {
	t.Helper()
	sum, err := o.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned an infrastructure error: %v", err)
	}
	return sum
}

// ---- tests ------------------------------------------------------------------------

// The headline happy path: one plan loop seeds the tasks and reports the plan
// complete, the build drain takes every task to done, and the run-level test gate is
// green → DONE with an accurate summary.
func TestRunHappyPath(t *testing.T) {
	fd := &fakeDriver{
		planComplete: incompleteCoverage,
		planIterate: func(f *fakeDriver, _ int) (*loop.Result, error) {
			if err := f.seedPending("0001", "0002"); err != nil {
				return nil, err
			}
			return planRes(true), nil
		},
	}
	o := setup(t, fd)

	sum := run(t, o)
	if sum.RunState != state.StateDone {
		t.Fatalf("RunState = %q, want DONE (halt: %q)", sum.RunState, sum.HaltReason)
	}
	if sum.TasksDone != 2 {
		t.Errorf("TasksDone = %d, want 2", sum.TasksDone)
	}
	if fd.planN != 1 {
		t.Errorf("plan loops = %d, want exactly 1", fd.planN)
	}
	if fd.runN != 3 { // 0001 done, 0002 done, NoWork
		t.Errorf("build loops = %d, want 3 (two tasks + the drain-exhausted no-work)", fd.runN)
	}
	if o.st.Phase != state.PhaseBuild {
		t.Errorf("final phase = %q, want build", o.st.Phase)
	}
	// state.json was persisted with the terminal run-state.
	onDisk, err := state.Load(o.statePath)
	if err != nil {
		t.Fatalf("load persisted state: %v", err)
	}
	if onDisk.RunState != state.StateDone {
		t.Errorf("persisted run_state = %q, want DONE", onDisk.RunState)
	}
}

// The defining spec-06 behavior: a drain that surfaces TWO blocked tasks triggers
// exactly ONE focused re-plan at the drain boundary (not one per blocked task — "at
// most one phase switch per drain boundary"), which resolves the blocks; build then
// resumes and completes.
func TestDrainThenSingleBatchReplan(t *testing.T) {
	fd := &fakeDriver{
		planComplete: incompleteCoverage,
		planIterate: func(f *fakeDriver, _ int) (*loop.Result, error) {
			switch f.planN {
			case 1: // initial plan: four tasks
				for _, id := range []string{"0001", "0002", "0003", "0004"} {
					if err := f.seedPending(id); err != nil {
						return nil, err
					}
				}
				return planRes(true), nil
			default: // the single focused re-plan: resolve both blocked tasks at once
				if err := f.resolveToPending("0002", "0004"); err != nil {
					return nil, err
				}
				return planRes(true), nil
			}
		},
		buildOutcome: map[string]task.Status{
			"0001": task.StatusDone,
			"0002": task.StatusBlocked,
			"0003": task.StatusDone,
			"0004": task.StatusBlocked,
		},
		buildReason: map[string]task.Reason{
			"0002": task.ReasonNewScope,
			"0004": task.ReasonNewScope,
		},
	}
	o := setup(t, fd)

	sum := run(t, o)
	if sum.RunState != state.StateDone {
		t.Fatalf("RunState = %q, want DONE (halt: %q)", sum.RunState, sum.HaltReason)
	}
	if fd.planN != 2 {
		t.Errorf("plan loops = %d, want 2 (one initial + exactly one batch re-plan, not one per blocked task)", fd.planN)
	}
	if sum.TasksDone != 4 {
		t.Errorf("TasksDone = %d, want 4", sum.TasksDone)
	}
	if sum.TasksBlocked != 0 {
		t.Errorf("TasksBlocked = %d, want 0 (re-plan resolved them)", sum.TasksBlocked)
	}
	if o.st.Iter.Plan != 2 {
		t.Errorf("state.iter.plan = %d, want 2", o.st.Iter.Plan)
	}
}

// A re-plan that resolves nothing must not spin: the orchestrator halts when a
// focused re-plan leaves no actionable work.
func TestReplanNoProgressHalts(t *testing.T) {
	fd := &fakeDriver{
		planComplete: incompleteCoverage,
		planIterate: func(f *fakeDriver, _ int) (*loop.Result, error) {
			if f.planN == 1 {
				if err := f.seedPending("0001", "0002"); err != nil {
					return nil, err
				}
			}
			// The re-plan (call 2) deliberately resolves nothing.
			return planRes(true), nil
		},
		buildOutcome: map[string]task.Status{"0001": task.StatusBlocked, "0002": task.StatusBlocked},
		buildReason:  map[string]task.Reason{"0001": task.ReasonNewScope, "0002": task.ReasonNewScope},
	}
	o := setup(t, fd)

	sum := run(t, o)
	if sum.RunState != state.StateHalted {
		t.Fatalf("RunState = %q, want HALTED", sum.RunState)
	}
	if !strings.Contains(sum.HaltReason, "no new actionable") {
		t.Errorf("HaltReason = %q, want the no-progress re-plan reason", sum.HaltReason)
	}
	if fd.planN != 2 {
		t.Errorf("plan loops = %d, want 2 (initial + one futile re-plan, then stop)", fd.planN)
	}
}

// All tasks report done but the harness test gate is red → the false-done backstop
// halts rather than declaring a hollow success (spec 01 §done-detection).
func TestFalseDoneHalts(t *testing.T) {
	fd := &fakeDriver{} // plan already complete (default), drive the seeded task to done
	o := setup(t, fd)
	if err := fd.seedPending("0001"); err != nil {
		t.Fatal(err)
	}
	o.verify = func(_ context.Context, _ string, _ config.Commands) verify.Result {
		return verify.Result{Test: verify.CommandResult{Ran: true, ExitCode: 1}} // red
	}

	sum := run(t, o)
	if sum.RunState != state.StateHalted {
		t.Fatalf("RunState = %q, want HALTED", sum.RunState)
	}
	if !strings.Contains(sum.HaltReason, "false done") {
		t.Errorf("HaltReason = %q, want the false-done reason", sum.HaltReason)
	}
}

// A stall disposition from RunTask halts the run.
func TestStallHalts(t *testing.T) {
	fd := &fakeDriver{
		runTaskOverride: func(_ *fakeDriver, _, _ int) (*loop.TaskFlowResult, error) {
			return &loop.TaskFlowResult{Disposition: loop.DispStalled}, nil
		},
	}
	o := setup(t, fd)

	sum := run(t, o)
	if sum.RunState != state.StateHalted {
		t.Fatalf("RunState = %q, want HALTED", sum.RunState)
	}
	if !strings.Contains(sum.HaltReason, "stall") {
		t.Errorf("HaltReason = %q, want a stall reason", sum.HaltReason)
	}
}

// A max-iterations disposition from RunTask halts the run.
func TestMaxIterationsHalts(t *testing.T) {
	fd := &fakeDriver{
		runTaskOverride: func(_ *fakeDriver, _, _ int) (*loop.TaskFlowResult, error) {
			return &loop.TaskFlowResult{Disposition: loop.DispMaxIterations}, nil
		},
	}
	o := setup(t, fd)

	sum := run(t, o)
	if sum.RunState != state.StateHalted {
		t.Fatalf("RunState = %q, want HALTED", sum.RunState)
	}
	if !strings.Contains(sum.HaltReason, "max iterations") {
		t.Errorf("HaltReason = %q, want a max-iterations reason", sum.HaltReason)
	}
}

// A usage limit with a reset already in the past waits for ~0 and resumes, then the
// run completes — and one window is counted (spec 01/09; src/lib/usage).
func TestUsageLimitWaitsThenResumes(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	fd := &fakeDriver{}
	fd.runTaskOverride = func(f *fakeDriver, iter, stall int) (*loop.TaskFlowResult, error) {
		if f.runN == 1 {
			res := &loop.Result{Outcome: stream.OutcomeUsageLimit, Observation: &stream.LoopObservation{ResetAt: &past}}
			return &loop.TaskFlowResult{Disposition: loop.DispUsageLimit, Iterations: 1, StallCount: stall, Results: []*loop.Result{res}, Last: res}, nil
		}
		return f.driveNext(stall)
	}
	o := setup(t, fd)
	if err := fd.seedPending("0001"); err != nil {
		t.Fatal(err)
	}

	sum := run(t, o)
	if sum.RunState != state.StateDone {
		t.Fatalf("RunState = %q, want DONE (halt: %q)", sum.RunState, sum.HaltReason)
	}
	if o.st.Usage.CyclesUsed != 1 {
		t.Errorf("usage.cycles_used = %d, want 1 (one window entered)", o.st.Usage.CyclesUsed)
	}
}

// With [usage].on_limit=halt, a usage limit halts immediately instead of waiting.
func TestUsageLimitHaltMode(t *testing.T) {
	fd := &fakeDriver{
		runTaskOverride: func(_ *fakeDriver, _, stall int) (*loop.TaskFlowResult, error) {
			res := &loop.Result{Outcome: stream.OutcomeUsageLimit, Observation: &stream.LoopObservation{}}
			return &loop.TaskFlowResult{Disposition: loop.DispUsageLimit, Iterations: 1, StallCount: stall, Results: []*loop.Result{res}, Last: res}, nil
		},
	}
	o := setup(t, fd)
	o.waiter = usage.NewWaiter(config.Usage{OnLimit: "halt"}, nil)

	sum := run(t, o)
	if sum.RunState != state.StateHalted {
		t.Fatalf("RunState = %q, want HALTED", sum.RunState)
	}
	if !strings.Contains(sum.HaltReason, "on_limit=halt") {
		t.Errorf("HaltReason = %q, want the on_limit=halt reason", sum.HaltReason)
	}
}

// A restored DONE state is reported as-is without running anything (spec 09 §resume).
func TestResumeDoneStateNoOp(t *testing.T) {
	fd := &fakeDriver{}
	o := setup(t, fd)
	o.st.RunState = state.StateDone

	sum := run(t, o)
	if sum.RunState != state.StateDone {
		t.Fatalf("RunState = %q, want DONE", sum.RunState)
	}
	if fd.planN != 0 || fd.runN != 0 {
		t.Errorf("driver was called on a restored DONE state (plan=%d run=%d), want none", fd.planN, fd.runN)
	}
}

// A restored HALTED state is not auto-resumed (the guardrail halt is the operator's
// to clear); it is reported as-is (spec 09).
func TestResumeHaltedStateNoAutoRetry(t *testing.T) {
	fd := &fakeDriver{}
	o := setup(t, fd)
	o.st.RunState = state.StateHalted
	o.st.Halt = state.Halt{Reason: "prior stall", Task: "0007"}

	sum := run(t, o)
	if sum.RunState != state.StateHalted {
		t.Fatalf("RunState = %q, want HALTED", sum.RunState)
	}
	if sum.HaltReason != "prior stall" {
		t.Errorf("HaltReason = %q, want the restored reason", sum.HaltReason)
	}
	if fd.planN != 0 || fd.runN != 0 {
		t.Errorf("driver was called on a restored HALTED state, want none")
	}
}

// On a crash mid-loop (RUNNING on disk) the orchestrator reconciles ground truth
// before continuing: an `active` task is normalized to pending so it is selectable
// again. Without that step the selector would skip the active task and the run could
// never finish; reaching DONE proves the normalization happened.
func TestResumeRunningReconcilesActiveTask(t *testing.T) {
	fd := &fakeDriver{} // plan already complete; drive the (normalized) task to done
	o := setup(t, fd)
	o.st.RunState = state.StateRunning
	if err := writeTaskFile(fd.tasksDir, "0001", task.StatusActive, ""); err != nil {
		t.Fatal(err)
	}

	sum := run(t, o)
	if sum.RunState != state.StateDone {
		t.Fatalf("RunState = %q, want DONE — the active task should have been normalized to pending and built (halt: %q)", sum.RunState, sum.HaltReason)
	}
	if sum.TasksDone != 1 {
		t.Errorf("TasksDone = %d, want 1", sum.TasksDone)
	}
}

// The plan phase stalls (consecutive no-progress plan loops never reach completeness)
// → HALTED. StallN is 3 by default, so three no-progress loops trip it.
func TestPlanPhaseStallHalts(t *testing.T) {
	fd := &fakeDriver{
		planComplete: incompleteCoverage,
		planIterate: func(_ *fakeDriver, _ int) (*loop.Result, error) {
			// Never complete, never touch files → no progress every loop.
			r := planRes(false)
			r.WorkHappened = false
			return r, nil
		},
	}
	o := setup(t, fd)

	sum := run(t, o)
	if sum.RunState != state.StateHalted {
		t.Fatalf("RunState = %q, want HALTED", sum.RunState)
	}
	if !strings.Contains(sum.HaltReason, "stall") {
		t.Errorf("HaltReason = %q, want a plan-stall reason", sum.HaltReason)
	}
	if fd.planN != o.cfg.Guardrails.StallN {
		t.Errorf("plan loops = %d, want stall_n=%d before halting", fd.planN, o.cfg.Guardrails.StallN)
	}
}
