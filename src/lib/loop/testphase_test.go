package loop

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"flanders/src/lib/reconcile"
	"flanders/src/lib/stream"
	"flanders/src/lib/supervise"
	"flanders/src/lib/task"
)

// TestClassifyTestVerdict locks the pure branch table that maps (post-reconcile status,
// gate result) → the test-phase routing decision (spec 07 §test agent).
func TestClassifyTestVerdict(t *testing.T) {
	cases := []struct {
		name       string
		status     task.Status
		gateRan    bool
		gatePassed bool
		want       TestVerdict
	}{
		// Branch 2: agent marked done, gate corroborates → satisfied (skip build).
		{"done+green=satisfied", task.StatusDone, true, true, TestVerdictSatisfied},
		// Unconfirmed done: agent said done but suite red → cannot confirm → proceed to build.
		{"done+red=redready", task.StatusDone, true, false, TestVerdictRedReady},
		// No gate ran (no test command): only the agent's word → satisfied.
		{"done+nogate=satisfied", task.StatusDone, false, false, TestVerdictSatisfied},
		// Branches 1/3: a red acceptance test exists → build it.
		{"pending+red=redready", task.StatusPending, true, false, TestVerdictRedReady},
		// Degraded: no red test produced and satisfaction not claimed → re-run test loop.
		{"pending+green=noredtest", task.StatusPending, true, true, TestVerdictNoRedTest},
		{"pending+nogate=noredtest", task.StatusPending, false, false, TestVerdictNoRedTest},
		// Blocked is the drain/re-plan path regardless of the gate.
		{"blocked=blocked", task.StatusBlocked, true, false, TestVerdictBlocked},
		{"blocked+green=blocked", task.StatusBlocked, true, true, TestVerdictBlocked},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyTestVerdict(c.status, c.gateRan, c.gatePassed); got != c.want {
				t.Errorf("classifyTestVerdict(%q, ran=%v, passed=%v) = %q, want %q",
					c.status, c.gateRan, c.gatePassed, got, c.want)
			}
		})
	}
}

// TestComposeTestPromptIsTestAgentContract: the test-phase prompt carries the TDD
// test-agent contract (ensure a red test, never implement, the three branches) and not
// the build implementer instruction — the test/implementer split is what makes the gate
// trustworthy (spec 07).
func TestComposeTestPromptIsTestAgentContract(t *testing.T) {
	d, _ := newComposeDriver(t)
	store, err := task.NewStore([]*task.Task{mkTask("0001", task.StatusPending, nil)})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	testPrompt, err := d.composePrompt(store.ByID("0001"), store, "test")
	if err != nil {
		t.Fatalf("composePrompt(test): %v", err)
	}
	for _, want := range []string{"You are the TEST agent", "FAILING (red) acceptance test", "Do not write production code", "skip the build loop"} {
		if !strings.Contains(testPrompt, want) {
			t.Errorf("test prompt missing %q:\n%s", want, testPrompt)
		}
	}
	if strings.Contains(testPrompt, "You are the BUILD agent") {
		t.Errorf("test prompt leaked the build role:\n%s", testPrompt)
	}
	// The shared body is still present (it is the same selected task).
	if !strings.Contains(testPrompt, "Current task: 0001") {
		t.Errorf("test prompt missing the shared task header:\n%s", testPrompt)
	}

	buildPrompt, err := d.composePrompt(store.ByID("0001"), store, "build")
	if err != nil {
		t.Fatalf("composePrompt(build): %v", err)
	}
	if !strings.Contains(buildPrompt, "You are the BUILD agent") {
		t.Errorf("build prompt missing the build role:\n%s", buildPrompt)
	}
	if strings.Contains(buildPrompt, "You are the TEST agent") {
		t.Errorf("build prompt leaked the test role:\n%s", buildPrompt)
	}
}

// TestTestIterateRedReadyProceedsToBuild: the test agent leaves the task pending (a red
// acceptance test now exists) and the harness gate is RED → verdict RedReady, the task
// stays selectable for the build loop (spec 07 branches 1/3).
func TestTestIterateRedReadyProceedsToBuild(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Commands.Test = "exit 1" // a red acceptance test now fails the suite
	taskPath := filepath.Join(p.Tasks, "0001.md")
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = stubSuccess // test agent ran cleanly, left status pending

	res, err := d.Iterate(context.Background(), "test", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.TestVerdict != TestVerdictRedReady {
		t.Errorf("TestVerdict = %q, want red-ready", res.TestVerdict)
	}
	if after, _ := task.ParseFile(taskPath); after.Status() != task.StatusPending {
		t.Errorf("on-disk status = %q, want pending (selectable for build)", after.Status())
	}
}

// TestTestIterateAlreadySatisfiedSkipsBuild: the test agent finds the acceptance already
// satisfied and marks the task done; the harness gate is GREEN, corroborating it → verdict
// Satisfied and the task stays done (spec 07 branch 2 — the build loop is skipped).
func TestTestIterateAlreadySatisfiedSkipsBuild(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Commands.Test = "exit 0" // a passing test already covers the acceptance
	taskPath := filepath.Join(p.Tasks, "0001.md")
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = func(_ context.Context, spec supervise.Spec) (*supervise.Result, error) {
		// The test agent reports the acceptance is already satisfied by flipping status.
		tk, _ := task.ParseFile(taskPath)
		tk.SetStatus(task.StatusDone)
		tk.SetNotes("covered by existing TestFoo")
		if err := tk.WriteFile(taskPath); err != nil {
			t.Fatalf("agent write: %v", err)
		}
		if spec.RawSink != nil {
			_, _ = spec.RawSink.Write([]byte(`{"type":"result","subtype":"success"}` + "\n"))
		}
		return &supervise.Result{Observation: &stream.LoopObservation{Done: true, Subtype: "success"}, ExitCode: 0}, nil
	}

	res, err := d.Iterate(context.Background(), "test", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.TestVerdict != TestVerdictSatisfied {
		t.Errorf("TestVerdict = %q, want satisfied", res.TestVerdict)
	}
	if res.Reconcile.Action != reconcile.ActionRespected || res.Reconcile.To != task.StatusDone {
		t.Errorf("Reconcile = %+v, want Respected→done (agent's done honored)", res.Reconcile)
	}
	if after, _ := task.ParseFile(taskPath); after.Status() != task.StatusDone {
		t.Errorf("on-disk status = %q, want done (acceptance already satisfied)", after.Status())
	}
}

// TestTestIterateGreenGateDoesNotPromote: the headline test-phase difference from build.
// The test agent left the task pending and the suite is GREEN — but unlike the build phase
// the harness does NOT promote it to done (a green suite can mean "no test covers this
// acceptance yet"). The verdict is NoRedTest so the orchestrator re-runs the test loop.
func TestTestIterateGreenGateDoesNotPromote(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Commands.Test = "exit 0" // suite green, but the agent wrote no red test
	taskPath := filepath.Join(p.Tasks, "0001.md")
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = stubSuccess // agent leaves status pending

	res, err := d.Iterate(context.Background(), "test", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.TestVerdict != TestVerdictNoRedTest {
		t.Errorf("TestVerdict = %q, want no-red-test", res.TestVerdict)
	}
	if res.Reconcile.Action == reconcile.ActionPromoted {
		t.Errorf("Reconcile promoted on a green gate in the test phase — must not (could be no test)")
	}
	if after, _ := task.ParseFile(taskPath); after.Status() != task.StatusPending {
		t.Errorf("on-disk status = %q, want pending (NOT promoted to done)", after.Status())
	}
}

// TestTestIterateUnconfirmedDoneDemoted: the test agent claims the acceptance is satisfied
// (status done) but the harness gate is RED, so the claim is unconfirmed — the harness
// demotes it to pending and routes to build (ground truth, not agent self-report; spec 00
// decision 2), rather than skipping build on an unverified done.
func TestTestIterateUnconfirmedDoneDemoted(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Commands.Test = "exit 1" // suite is red — cannot confirm the agent's done
	taskPath := filepath.Join(p.Tasks, "0001.md")
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = func(_ context.Context, spec supervise.Spec) (*supervise.Result, error) {
		tk, _ := task.ParseFile(taskPath)
		tk.SetStatus(task.StatusDone) // agent overclaims satisfaction
		if err := tk.WriteFile(taskPath); err != nil {
			t.Fatalf("agent write: %v", err)
		}
		if spec.RawSink != nil {
			_, _ = spec.RawSink.Write([]byte(`{"type":"result","subtype":"success"}` + "\n"))
		}
		return &supervise.Result{Observation: &stream.LoopObservation{Done: true, Subtype: "success"}, ExitCode: 0}, nil
	}

	res, err := d.Iterate(context.Background(), "test", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.TestVerdict != TestVerdictRedReady {
		t.Errorf("TestVerdict = %q, want red-ready (unconfirmed done → build)", res.TestVerdict)
	}
	if after, _ := task.ParseFile(taskPath); after.Status() != task.StatusPending {
		t.Errorf("on-disk status = %q, want pending (unconfirmed done demoted)", after.Status())
	}
	if res.Reconcile.To != task.StatusPending {
		t.Errorf("Reconcile.To = %q, want pending", res.Reconcile.To)
	}
}

// TestTestIterateBlockedDrains: an agent block in the test phase (e.g. the acceptance
// cannot be tested without a missing dependency) is respected and surfaced as the Blocked
// verdict so the orchestrator routes it to drain/re-plan, not the build loop.
func TestTestIterateBlockedDrains(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Commands.Test = "exit 1"
	taskPath := filepath.Join(p.Tasks, "0001.md")
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = func(_ context.Context, spec supervise.Spec) (*supervise.Result, error) {
		tk, _ := task.ParseFile(taskPath)
		tk.SetBlocked(task.ReasonDependency)
		if err := tk.WriteFile(taskPath); err != nil {
			t.Fatalf("agent write: %v", err)
		}
		if spec.RawSink != nil {
			_, _ = spec.RawSink.Write([]byte(`{"type":"result","subtype":"success"}` + "\n"))
		}
		return &supervise.Result{Observation: &stream.LoopObservation{Done: true, Subtype: "success"}, ExitCode: 0}, nil
	}

	res, err := d.Iterate(context.Background(), "test", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.TestVerdict != TestVerdictBlocked {
		t.Errorf("TestVerdict = %q, want blocked", res.TestVerdict)
	}
	if res.Reconcile.Action != reconcile.ActionRespected || res.Reconcile.To != task.StatusBlocked {
		t.Errorf("Reconcile = %+v, want Respected→blocked", res.Reconcile)
	}
}

// TestTestIterateNonSuccessNoVerdict: a test loop that did not complete cleanly (an error
// result) yields no verdict — the orchestrator routes that on Result.Outcome, not a
// TestVerdict (and the gate is skipped, as for any non-success loop).
func TestTestIterateNonSuccessNoVerdict(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Commands.Test = "exit 0"
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = func(_ context.Context, _ supervise.Spec) (*supervise.Result, error) {
		return &supervise.Result{Observation: &stream.LoopObservation{IsError: true}, ExitCode: 1}, nil
	}

	res, err := d.Iterate(context.Background(), "test", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.Outcome != stream.OutcomeError {
		t.Fatalf("Outcome = %v, want error (precondition)", res.Outcome)
	}
	if res.TestVerdict != TestVerdictNone {
		t.Errorf("TestVerdict = %q, want none (non-success test loop)", res.TestVerdict)
	}
}
