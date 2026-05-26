package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flanders/src/lib/stream"
	"flanders/src/lib/supervise"
	"flanders/src/lib/task"
)

// phaseAwareRun returns a d.run stub that dispatches on the composed prompt's role header
// (the test agent vs the build agent — the strings compose.go emits) so a single flow test
// can drive both phases. Each side runs the supplied callback (to mutate the project as
// that agent would) and returns a clean success observation.
func phaseAwareRun(t *testing.T, onTest, onBuild func()) func(context.Context, supervise.Spec) (*supervise.Result, error) {
	t.Helper()
	return func(_ context.Context, spec supervise.Spec) (*supervise.Result, error) {
		switch {
		case strings.Contains(spec.Prompt, "You are the TEST agent"):
			if onTest != nil {
				onTest()
			}
		case strings.Contains(spec.Prompt, "You are the BUILD agent"):
			if onBuild != nil {
				onBuild()
			}
		default:
			t.Fatalf("prompt has neither TEST nor BUILD role header:\n%s", spec.Prompt)
		}
		if spec.RawSink != nil {
			_, _ = spec.RawSink.Write([]byte(`{"type":"result","subtype":"success"}` + "\n"))
		}
		return &supervise.Result{
			Observation: &stream.LoopObservation{Done: true, Subtype: "success"},
			ExitCode:    0,
		}, nil
	}
}

// TestRunTaskRedToGreenVerified is the headline 4.5 acceptance: a task drives
// red → green → verified. The test agent leaves a red acceptance test (the harness gate is
// RED → RedReady, build is NOT skipped); the build agent implements (the gate turns GREEN);
// the harness promotes the task to done and the flow reports a gate-verified DispDone.
func TestRunTaskRedToGreenVerified(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	// A gate that is red until the build agent creates the sentinel — the ground-truth
	// red→green signal, run by verify.Run as `sh -c` in the project root.
	cfg.Commands.Test = "test -f built"
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = phaseAwareRun(t,
		func() { // test agent: write a red acceptance test, do NOT satisfy the gate
			mustWrite(t, filepath.Join(p.Root, "feature_test.go"), "package main\n")
		},
		func() { // build agent: implement → the gate now passes
			mustWrite(t, filepath.Join(p.Root, "built"), "ok\n")
		},
	)

	out, err := d.RunTask(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if out.Disposition != DispDone {
		t.Fatalf("Disposition = %q, want done (red→green→verified)", out.Disposition)
	}
	if !out.Verified {
		t.Error("Verified = false, want true (the gate corroborated done)")
	}
	if out.TestVerdict != TestVerdictRedReady {
		t.Errorf("TestVerdict = %q, want red-ready (build was not skipped)", out.TestVerdict)
	}
	if out.Iterations != 2 {
		t.Errorf("Iterations = %d, want 2 (one test loop + one build loop)", out.Iterations)
	}
	if after, _ := task.ParseFile(filepath.Join(p.Tasks, "0001.md")); after.Status() != task.StatusDone {
		t.Errorf("on-disk status = %q, want done", after.Status())
	}
}

// TestRunTaskSatisfiedSkipsBuild: the test agent finds the acceptance already satisfied
// (status done) and the gate is green → DispDone after a SINGLE loop; the build agent is
// never invoked (spec 07 branch 2).
func TestRunTaskSatisfiedSkipsBuild(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Commands.Test = "exit 0" // a passing test already covers the acceptance
	taskPath := filepath.Join(p.Tasks, "0001.md")
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	buildRan := false
	d.run = phaseAwareRun(t,
		func() { // test agent reports satisfaction by flipping status
			tk, _ := task.ParseFile(taskPath)
			tk.SetStatus(task.StatusDone)
			if werr := tk.WriteFile(taskPath); werr != nil {
				t.Fatalf("agent write: %v", werr)
			}
		},
		func() { buildRan = true },
	)

	out, err := d.RunTask(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if out.Disposition != DispDone || out.TestVerdict != TestVerdictSatisfied {
		t.Errorf("Disposition=%q TestVerdict=%q, want done/satisfied", out.Disposition, out.TestVerdict)
	}
	if buildRan {
		t.Error("build agent ran, want skipped (acceptance already satisfied)")
	}
	if out.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1 (test loop only)", out.Iterations)
	}
}

// TestRunTaskBlockedDrains: the test agent blocks the task (e.g. a missing dependency) →
// DispBlocked carrying the reason so the orchestrator routes it (new-scope → re-plan), and
// the build loop is not entered (spec 06 §drain).
func TestRunTaskBlockedDrains(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Commands.Test = "exit 1"
	taskPath := filepath.Join(p.Tasks, "0001.md")
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	buildRan := false
	d.run = phaseAwareRun(t,
		func() {
			tk, _ := task.ParseFile(taskPath)
			tk.SetBlocked(task.ReasonNewScope)
			if werr := tk.WriteFile(taskPath); werr != nil {
				t.Fatalf("agent write: %v", werr)
			}
		},
		func() { buildRan = true },
	)

	out, err := d.RunTask(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if out.Disposition != DispBlocked || out.Reason != task.ReasonNewScope {
		t.Errorf("Disposition=%q Reason=%q, want blocked/new-scope", out.Disposition, out.Reason)
	}
	if buildRan {
		t.Error("build agent ran, want not entered on a blocked task")
	}
}

// TestRunTaskNoWork: with every task done, the first (test) Iterate reports NoWork+AllDone
// — the flow returns DispNoWork with AllDone, having spawned nothing (Iterations 0).
func TestRunTaskNoWork(t *testing.T) {
	cfg, p, jr := setupProject(t,
		mkTask("0001", task.StatusDone, nil),
		mkTask("0002", task.StatusDone, []string{"0001"}),
	)
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	spawned := false
	d.run = func(context.Context, supervise.Spec) (*supervise.Result, error) {
		spawned = true
		return nil, nil
	}
	out, err := d.RunTask(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if out.Disposition != DispNoWork || !out.AllDone {
		t.Errorf("Disposition=%q AllDone=%v, want no-work/true", out.Disposition, out.AllDone)
	}
	if spawned || out.Iterations != 0 {
		t.Errorf("spawned=%v Iterations=%d, want no spawn / 0 iterations", spawned, out.Iterations)
	}
}

// TestRunTaskNoRedTestRetriesThenGivesUp: the test agent never writes a red test and never
// claims satisfaction (the suite stays green, status stays pending) → NoRedTest each loop.
// The flow retries up to defaultMaxTestAttempts then surfaces DispNoRedTest rather than
// building against no red test (the TDD invariant).
func TestRunTaskNoRedTestRetriesThenGivesUp(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Commands.Test = "exit 0" // green suite, but the agent produces no red test
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = phaseAwareRun(t, func() {}, func() { t.Fatal("build ran, want no build without a red test") })

	out, err := d.RunTask(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if out.Disposition != DispNoRedTest {
		t.Errorf("Disposition = %q, want no-red-test", out.Disposition)
	}
	if out.Iterations != defaultMaxTestAttempts {
		t.Errorf("Iterations = %d, want %d (bounded test retries)", out.Iterations, defaultMaxTestAttempts)
	}
}

// TestRunTaskStalls: a red test is ready but the build agent makes no progress (no files,
// status stays pending, gate stays red). With a small stall_n the consecutive-no-progress
// counter trips and the flow returns DispStalled rather than looping forever.
func TestRunTaskStalls(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Commands.Test = "exit 1" // gate stays red → task never promotes
	cfg.Guardrails.StallN = 2     // trip quickly: test loop (no progress) + one stuck build loop
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = phaseAwareRun(t,
		func() {}, // test agent: leaves status pending, writes nothing → RedReady (gate red)
		func() {}, // build agent: makes no progress
	)

	out, err := d.RunTask(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if out.Disposition != DispStalled {
		t.Errorf("Disposition = %q, want stalled", out.Disposition)
	}
	if out.StallCount < cfg.Guardrails.StallN {
		t.Errorf("StallCount = %d, want >= stall_n %d", out.StallCount, cfg.Guardrails.StallN)
	}
}

// TestRunTaskMaxIterations: the per-phase iteration cap stops the flow. With max_iterations
// 1, the test loop consumes the only allowed iteration and the build loop is refused before
// spawning → DispMaxIterations.
func TestRunTaskMaxIterations(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Commands.Test = "exit 1" // red → RedReady, so the flow wants to build
	cfg.Guardrails.MaxIterations = 1
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = phaseAwareRun(t,
		func() {},
		func() { t.Fatal("build ran, want refused at the iteration cap") },
	)

	out, err := d.RunTask(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if out.Disposition != DispMaxIterations {
		t.Errorf("Disposition = %q, want max-iterations", out.Disposition)
	}
	if out.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1 (test loop only; build refused)", out.Iterations)
	}
}

// TestRunTaskUsageLimitReturns: a loop that hits the usage limit ends the flow with
// DispUsageLimit (the orchestrator waits + resumes, then re-enters) rather than being
// treated as a generic error.
func TestRunTaskUsageLimitReturns(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = func(_ context.Context, _ supervise.Spec) (*supervise.Result, error) {
		return &supervise.Result{Observation: &stream.LoopObservation{UsageLimited: true}, ExitCode: 1}, nil
	}

	out, err := d.RunTask(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if out.Disposition != DispUsageLimit {
		t.Errorf("Disposition = %q, want usage-limit", out.Disposition)
	}
}

// TestDispositionForOutcome locks the pure non-success → disposition mapping, including that
// a timeout (folded into OutcomeError by Classify) is reported distinctly via Result.TimedOut.
func TestDispositionForOutcome(t *testing.T) {
	cases := []struct {
		name string
		res  *Result
		want Disposition
	}{
		{"timeout-wins-over-error", &Result{Outcome: stream.OutcomeError, TimedOut: true}, DispTimeout},
		{"usage-limit", &Result{Outcome: stream.OutcomeUsageLimit}, DispUsageLimit},
		{"error", &Result{Outcome: stream.OutcomeError}, DispError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dispositionForOutcome(c.res); got != c.want {
				t.Errorf("dispositionForOutcome(%+v) = %q, want %q", c.res, got, c.want)
			}
		})
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
