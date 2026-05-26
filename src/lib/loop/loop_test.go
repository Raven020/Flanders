package loop

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flanders/src/lib/config"
	"flanders/src/lib/invoke"
	"flanders/src/lib/journal"
	"flanders/src/lib/paths"
	"flanders/src/lib/reconcile"
	"flanders/src/lib/rules"
	"flanders/src/lib/stream"
	"flanders/src/lib/supervise"
	"flanders/src/lib/task"
)

// setupProject builds a temp project (config + resolved paths + open journal) and
// writes the given tasks under specs/tasks/. It is the common fixture for the
// driver tests: a self-contained project on disk the driver reads exactly as it
// would in production.
func setupProject(t *testing.T, tasks ...*task.Task) (*config.Config, *paths.Paths, *journal.Journal) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	// These shared-fixture tests are not about checkpointing (and run on a non-repo
	// temp dir), so disable it here; the dedicated checkpoint_test.go opts back in
	// with a real repo. The read-side git signal (WorkHappened/FilesTouched) is
	// independent of this flag, so the git-signal tests below are unaffected.
	cfg.Git.Enabled = false
	p, err := paths.NewFromConfig(root, &cfg)
	if err != nil {
		t.Fatalf("paths.NewFromConfig: %v", err)
	}
	if err := p.EnsureFlanders(); err != nil {
		t.Fatalf("EnsureFlanders: %v", err)
	}
	if err := os.MkdirAll(p.Tasks, 0o755); err != nil {
		t.Fatalf("mkdir tasks: %v", err)
	}
	for _, tk := range tasks {
		if err := tk.WriteFile(filepath.Join(p.Tasks, tk.ID()+".md")); err != nil {
			t.Fatalf("write task %s: %v", tk.ID(), err)
		}
	}
	jr, err := journal.Open(p.Journal)
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	return &cfg, p, jr
}

func mkTask(id string, status task.Status, deps []string) *task.Task {
	return task.New(id, status, deps, "go test ./... passes for "+id, "## Task "+id+"\n\nDo the thing.\n")
}

// TestIterateSelectsComposesSpawnsJournals is the headline acceptance: one
// iteration selects the lowest-id actionable task, composes a prompt naming it,
// builds a fresh-session invocation, runs it (stub), and writes a journal entry
// that round-trips with the observation's cost/tokens/session.
func TestIterateSelectsComposesSpawnsJournals(t *testing.T) {
	cfg, p, jr := setupProject(t,
		mkTask("0001", task.StatusPending, nil),
		mkTask("0002", task.StatusPending, []string{"0001"}), // gated on 0001 → not selected
	)
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.newSessionID = func() (string, error) { return "sess-fixed", nil }

	var gotSpec supervise.Spec
	d.run = func(_ context.Context, spec supervise.Spec) (*supervise.Result, error) {
		gotSpec = spec
		if spec.RawSink != nil {
			_, _ = spec.RawSink.Write([]byte(`{"type":"result","subtype":"success"}` + "\n"))
		}
		return &supervise.Result{
			Observation: &stream.LoopObservation{
				SessionID: "echoed-by-cli", Done: true, Subtype: "success", Cost: 0.05,
				FinalUsage: stream.Usage{InputTokens: 1000, OutputTokens: 200, CacheReadInputTokens: 50, CacheCreationInputTokens: 10},
				Subagents:  []stream.SubagentSpawn{{SubagentType: "Explore"}},
			},
			ExitCode: 0,
			Duration: 2 * time.Second,
		}, nil
	}

	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}

	// Selection: lowest-id pending whose deps are done → 0001 (0002 is gated).
	if res.NoWork || res.Task == nil || res.Task.ID() != "0001" {
		t.Fatalf("selected %v (NoWork=%v), want task 0001", res.Task, res.NoWork)
	}
	if res.Outcome != stream.OutcomeSuccess {
		t.Errorf("Outcome = %v, want success", res.Outcome)
	}
	if res.SessionID != "sess-fixed" {
		t.Errorf("SessionID = %q, want sess-fixed", res.SessionID)
	}
	if res.JournalSeq == 0 {
		t.Fatal("JournalSeq = 0, want a written entry")
	}

	// Compose: prompt names the task and never an empty turn.
	if !strings.Contains(gotSpec.Prompt, "Current task: 0001") {
		t.Errorf("prompt missing task id:\n%s", gotSpec.Prompt)
	}
	if !strings.Contains(gotSpec.Prompt, "0/2 tasks done") {
		t.Errorf("prompt missing plan summary:\n%s", gotSpec.Prompt)
	}

	// Spawn: fresh-session invocation, config-driven timeout + stream-input wiring.
	argv := strings.Join(gotSpec.Command.Argv(), " ")
	if gotSpec.Command.Bin != "claude" || !strings.Contains(argv, "--session-id sess-fixed") {
		t.Errorf("invocation wrong: %s", argv)
	}
	if gotSpec.StreamInput != cfg.Agent.StreamInput {
		t.Errorf("StreamInput = %v, want %v", gotSpec.StreamInput, cfg.Agent.StreamInput)
	}
	if gotSpec.Timeout != cfg.Guardrails.IterationTimeout.Duration {
		t.Errorf("Timeout = %v, want %v", gotSpec.Timeout, cfg.Guardrails.IterationTimeout.Duration)
	}

	// Journal round-trip: the entry reflects the observation and the phase class.
	sum, err := jr.Read(res.JournalSeq)
	if err != nil {
		t.Fatalf("journal.Read: %v", err)
	}
	if sum.Phase != "build" || sum.Task != "0001" || sum.SessionID != "sess-fixed" {
		t.Errorf("summary identity wrong: phase=%q task=%q session=%q", sum.Phase, sum.Task, sum.SessionID)
	}
	if sum.Model != "opus" || sum.Effort != "high" {
		t.Errorf("summary class wrong: model=%q effort=%q (want opus/high)", sum.Model, sum.Effort)
	}
	if sum.Cost != 0.05 || sum.Tokens.Input != 1000 || sum.Tokens.Output != 200 || sum.Tokens.CacheRead != 50 {
		t.Errorf("summary cost/tokens wrong: %+v tokens=%+v", sum.Cost, sum.Tokens)
	}
	if sum.StatusBefore != task.StatusPending || sum.StatusAfter != task.StatusPending {
		t.Errorf("status transition = %q→%q, want pending→pending (agent did not flip)", sum.StatusBefore, sum.StatusAfter)
	}
	if len(sum.Subagents) != 1 || sum.Subagents[0].Name != "Explore" {
		t.Errorf("subagents = %+v, want one Explore", sum.Subagents)
	}
	if sum.Test.Ran {
		t.Error("Test.Ran = true, want false (the verify step is plan task 3.4, not run here)")
	}

	// The verbatim transcript the runner emitted was archived alongside the summary.
	rc, err := jr.ReadStream(res.JournalSeq)
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	defer rc.Close()
	archived, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read archived stream: %v", err)
	}
	if !strings.Contains(string(archived), `"type":"result"`) {
		t.Errorf("archived stream missing emitted bytes: %q", archived)
	}
}

// TestIterateStatusAfterReflectsAgentEdit proves the journal captures the agent's
// own status flip: the driver re-reads the task file after the loop (the agent
// edits it mid-run — spec 02 §mutation ownership), so StatusAfter is `done` though
// the in-memory task was `pending` at selection.
func TestIterateStatusAfterReflectsAgentEdit(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	taskPath := filepath.Join(p.Tasks, "0001.md")

	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = func(_ context.Context, _ supervise.Spec) (*supervise.Result, error) {
		// Simulate the agent flipping its own status to done during the loop.
		tk, err := task.ParseFile(taskPath)
		if err != nil {
			t.Fatalf("agent reload: %v", err)
		}
		tk.SetStatus(task.StatusDone)
		if err := tk.WriteFile(taskPath); err != nil {
			t.Fatalf("agent write: %v", err)
		}
		return &supervise.Result{Observation: &stream.LoopObservation{Done: true, Subtype: "success"}, ExitCode: 0}, nil
	}

	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	sum, err := jr.Read(res.JournalSeq)
	if err != nil {
		t.Fatalf("journal.Read: %v", err)
	}
	if sum.StatusBefore != task.StatusPending || sum.StatusAfter != task.StatusDone {
		t.Errorf("status transition = %q→%q, want pending→done", sum.StatusBefore, sum.StatusAfter)
	}
}

// TestIterateNoWorkAllDone: when every task is done, Next returns nothing and the
// driver reports NoWork+AllDone (the success signal) without spawning or journaling.
func TestIterateNoWorkAllDone(t *testing.T) {
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
	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if !res.NoWork || !res.AllDone {
		t.Errorf("NoWork=%v AllDone=%v, want both true", res.NoWork, res.AllDone)
	}
	if res.JournalSeq != 0 || spawned {
		t.Errorf("no-work iteration spawned=%v journaled=%d, want neither", spawned, res.JournalSeq)
	}
}

// TestIterateNoWorkStalled: a blocked task is not actionable but the plan is not
// done — NoWork with AllDone false, the signal the orchestrator reads as a stall.
func TestIterateNoWorkStalled(t *testing.T) {
	blocked := mkTask("0001", task.StatusPending, nil)
	blocked.SetBlocked(task.ReasonNewScope)
	cfg, p, jr := setupProject(t, blocked)
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if !res.NoWork || res.AllDone {
		t.Errorf("NoWork=%v AllDone=%v, want NoWork && !AllDone", res.NoWork, res.AllDone)
	}
}

// TestIterateCycleErrors: a dependency cycle is a real error (it would otherwise
// masquerade as a finished plan), so Iterate fails rather than reporting NoWork.
func TestIterateCycleErrors(t *testing.T) {
	cfg, p, jr := setupProject(t,
		mkTask("0001", task.StatusPending, []string{"0002"}),
		mkTask("0002", task.StatusPending, []string{"0001"}),
	)
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := d.Iterate(context.Background(), "build", 1); err == nil {
		t.Fatal("Iterate over a dependency cycle returned no error")
	}
}

// TestIterateRealSupervisorArchivesStream exercises the real supervise→stream→
// journal path end-to-end over a stub `cat` command (no live `claude`, per plan
// 2.5/3.1): the journal archives the verbatim transcript and the summary reflects
// the folded observation.
func TestIterateRealSupervisorArchivesStream(t *testing.T) {
	const fixture = `{"type":"system","subtype":"init","session_id":"s1","model":"opus","permissionMode":"bypassPermissions","tools":[]}
{"type":"assistant","session_id":"s1","message":{"role":"assistant","model":"opus","content":[{"type":"text","text":"hello"}]}}
{"type":"result","subtype":"success","is_error":false,"session_id":"s1","total_cost_usd":0.02,"duration_ms":1234,"num_turns":1,"result":"done","usage":{"input_tokens":80,"output_tokens":12,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}
`
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	fixturePath := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(fixturePath, []byte(fixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Swap the built `claude` command for a `cat` of the fixture, but run it through
	// the REAL supervisor so RawSink→journal and the observation fold are exercised.
	d.run = func(ctx context.Context, spec supervise.Spec) (*supervise.Result, error) {
		spec.Command = invoke.Command{Bin: "cat", Args: []string{fixturePath}}
		spec.StreamInput = false
		return supervise.Run(ctx, spec)
	}

	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.Outcome != stream.OutcomeSuccess || res.ExitCode != 0 {
		t.Errorf("Outcome=%v ExitCode=%d, want success/0", res.Outcome, res.ExitCode)
	}

	// The archived stream is byte-faithful to what the process emitted.
	rc, err := jr.ReadStream(res.JournalSeq)
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	defer rc.Close()
	gotBytes, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read archived stream: %v", err)
	}
	if got := string(gotBytes); got != fixture {
		t.Errorf("archived stream mismatch:\n got %q\nwant %q", got, fixture)
	}

	sum, err := jr.Read(res.JournalSeq)
	if err != nil {
		t.Fatalf("journal.Read: %v", err)
	}
	if sum.Cost != 0.02 || sum.Tokens.Input != 80 || sum.Tokens.Output != 12 {
		t.Errorf("summary did not reflect the folded observation: cost=%v tokens=%+v", sum.Cost, sum.Tokens)
	}
	if sum.DurationMS != 1234 {
		t.Errorf("DurationMS = %d, want 1234 (from result.duration_ms)", sum.DurationMS)
	}
}

// stubSuccess is a d.run that emits a minimal success transcript and returns a
// clean (OutcomeSuccess) observation — the common case for the gate tests below.
func stubSuccess(_ context.Context, spec supervise.Spec) (*supervise.Result, error) {
	if spec.RawSink != nil {
		_, _ = spec.RawSink.Write([]byte(`{"type":"result","subtype":"success"}` + "\n"))
	}
	return &supervise.Result{
		Observation: &stream.LoopObservation{Done: true, Subtype: "success"},
		ExitCode:    0,
	}, nil
}

// TestIterateTestGatePasses is the task-3.4 acceptance: after a clean build loop the
// harness runs the configured test command and trusts its exit code. With `exit 0`
// the gate passes — surfaced on Result.Verify and recorded in the journal.
func TestIterateTestGatePasses(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Commands.Test = "exit 0"
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = stubSuccess

	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.Verify == nil || !res.Verify.Passed() {
		t.Fatalf("Verify = %+v, want a passing gate", res.Verify)
	}
	sum, err := jr.Read(res.JournalSeq)
	if err != nil {
		t.Fatalf("journal.Read: %v", err)
	}
	if !sum.Test.Ran || sum.Test.ExitCode != 0 || !sum.Test.Passed() {
		t.Errorf("journal Test = %+v, want Ran=true ExitCode=0 (passed)", sum.Test)
	}
	if sum.Test.Command != "exit 0" {
		t.Errorf("journal Test.Command = %q, want %q", sum.Test.Command, "exit 0")
	}
}

// TestIterateTestGateFails: a non-zero test exit is the ground-truth "not done"
// signal — the gate reflects the REAL exit code (the headline 3.4 acceptance),
// independent of the agent's success self-report.
func TestIterateTestGateFails(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Commands.Test = "exit 5"
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = stubSuccess // agent reports success…

	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.Verify == nil || res.Verify.Passed() {
		t.Fatalf("Verify = %+v, want a FAILING gate despite agent success", res.Verify)
	}
	sum, err := jr.Read(res.JournalSeq)
	if err != nil {
		t.Fatalf("journal.Read: %v", err)
	}
	if !sum.Test.Ran || sum.Test.ExitCode != 5 || sum.Test.Passed() {
		t.Errorf("journal Test = %+v, want Ran=true ExitCode=5 (not passed)", sum.Test)
	}
}

// TestIteratePlanPhaseSkipsGate: the test gate is for code-producing phases only.
// A plan loop never runs the test command — even one configured to fail — so its
// completion is judged elsewhere (plan-completeness), and the journal honestly
// records Test.Ran=false.
func TestIteratePlanPhaseSkipsGate(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Commands.Test = "exit 1" // would fail the gate if it ran
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = stubSuccess

	res, err := d.Iterate(context.Background(), "plan", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.Verify != nil {
		t.Errorf("Verify = %+v, want nil (plan phase does not run the test gate)", res.Verify)
	}
	sum, err := jr.Read(res.JournalSeq)
	if err != nil {
		t.Fatalf("journal.Read: %v", err)
	}
	if sum.Test.Ran {
		t.Errorf("journal Test.Ran = true, want false (gate skipped for plan phase)")
	}
}

// TestIterateNonSuccessSkipsGate: when the invocation itself did not complete
// cleanly (here an error result), the gate is skipped — there is no point verifying
// a half-done tree, and the orchestrator's guardrails handle the non-success path.
func TestIterateNonSuccessSkipsGate(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Commands.Test = "exit 0" // would PASS if it ran — proves the skip is outcome-driven
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = func(_ context.Context, _ supervise.Spec) (*supervise.Result, error) {
		// Non-zero exit with no usage-limit signal → OutcomeError.
		return &supervise.Result{Observation: &stream.LoopObservation{IsError: true}, ExitCode: 1}, nil
	}

	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.Outcome != stream.OutcomeError {
		t.Fatalf("Outcome = %v, want error (precondition for this test)", res.Outcome)
	}
	if res.Verify != nil {
		t.Errorf("Verify = %+v, want nil (gate skipped on a non-success invocation)", res.Verify)
	}
}

// initGitRepo turns dir into a git repo with a deterministic identity and commits
// the current tree, so a loop's snapshot starts from a clean baseline.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@flanders.local"},
		{"config", "user.name", "Flanders Test"},
		{"config", "commit.gpgsign", "false"},
		{"add", "-A"},
		{"commit", "-qm", "baseline"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestIterateReconcilePromotesOnPassingGate is the headline 3.5 acceptance: the
// agent finished the loop without flipping status (still pending), but the harness
// test gate passed — so the harness records the `done` the agent forgot, both on
// disk and in the journal. The outcome is recorded whether the agent flipped it or not.
func TestIterateReconcilePromotesOnPassingGate(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Commands.Test = "exit 0"
	taskPath := filepath.Join(p.Tasks, "0001.md")
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = stubSuccess // agent leaves status pending

	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.Reconcile.Action != reconcile.ActionPromoted || res.Reconcile.To != task.StatusDone {
		t.Errorf("Reconcile = %+v, want Promoted→done", res.Reconcile)
	}
	if after, _ := task.ParseFile(taskPath); after.Status() != task.StatusDone {
		t.Errorf("on-disk status = %q, want done (harness promoted it)", after.Status())
	}
	sum, err := jr.Read(res.JournalSeq)
	if err != nil {
		t.Fatalf("journal.Read: %v", err)
	}
	if sum.StatusBefore != task.StatusPending || sum.StatusAfter != task.StatusDone {
		t.Errorf("journal transition = %q→%q, want pending→done", sum.StatusBefore, sum.StatusAfter)
	}
}

// TestIterateReconcileNormalizesActive: the agent set the task `active` while
// working but never flipped on exit, and no test gate proved it done. Since the
// selector only re-picks `pending`, the harness normalizes the stuck `active` back
// to `pending` so the task is retried next loop.
func TestIterateReconcileNormalizesActive(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	taskPath := filepath.Join(p.Tasks, "0001.md")
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = func(_ context.Context, _ supervise.Spec) (*supervise.Result, error) {
		tk, err := task.ParseFile(taskPath)
		if err != nil {
			t.Fatalf("agent reload: %v", err)
		}
		tk.SetStatus(task.StatusActive) // agent marks active, then "crashes" without flipping
		if err := tk.WriteFile(taskPath); err != nil {
			t.Fatalf("agent write: %v", err)
		}
		return &supervise.Result{Observation: &stream.LoopObservation{Done: true, Subtype: "success"}, ExitCode: 0}, nil
	}

	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.Reconcile.Action != reconcile.ActionNormalized || res.Reconcile.To != task.StatusPending {
		t.Errorf("Reconcile = %+v, want Normalized→pending", res.Reconcile)
	}
	if after, _ := task.ParseFile(taskPath); after.Status() != task.StatusPending {
		t.Errorf("on-disk status = %q, want pending (normalized from active)", after.Status())
	}
}

// TestIterateRespectsAgentBlocked: an explicit agent block (with reason) is honored
// verbatim — the harness does not second-guess it, and the reason reaches the journal.
func TestIterateRespectsAgentBlocked(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	taskPath := filepath.Join(p.Tasks, "0001.md")
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = func(_ context.Context, _ supervise.Spec) (*supervise.Result, error) {
		tk, _ := task.ParseFile(taskPath)
		tk.SetBlocked(task.ReasonContextOverreach)
		if err := tk.WriteFile(taskPath); err != nil {
			t.Fatalf("agent write: %v", err)
		}
		return &supervise.Result{Observation: &stream.LoopObservation{Done: true, Subtype: "success"}, ExitCode: 0}, nil
	}

	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.Reconcile.Action != reconcile.ActionRespected || res.Reconcile.To != task.StatusBlocked {
		t.Errorf("Reconcile = %+v, want Respected→blocked", res.Reconcile)
	}
	sum, err := jr.Read(res.JournalSeq)
	if err != nil {
		t.Fatalf("journal.Read: %v", err)
	}
	if sum.StatusAfter != task.StatusBlocked || sum.Reason != task.ReasonContextOverreach {
		t.Errorf("journal = %q/%q, want blocked/context-overreach", sum.StatusAfter, sum.Reason)
	}
}

// TestIterateRecordsGitWork: when the target is a git repo, the harness records
// which files the loop touched and that work happened (spec 02 §Mutation ownership)
// — the git-diff signal status reconciliation and the stall guardrail (3.9) read.
// Surfaced on Result and recorded in the journal Files field.
func TestIterateRecordsGitWork(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	initGitRepo(t, p.Root) // commit the project (tasks/config) → clean baseline
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = func(_ context.Context, _ supervise.Spec) (*supervise.Result, error) {
		// The "agent" lands a new source file in the repo this loop.
		if err := os.WriteFile(filepath.Join(p.Root, "feature.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatalf("agent write source: %v", err)
		}
		return &supervise.Result{Observation: &stream.LoopObservation{Done: true, Subtype: "success"}, ExitCode: 0}, nil
	}

	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if !res.WorkHappened {
		t.Error("WorkHappened = false, want true (a source file was added)")
	}
	if !contains(res.FilesTouched, "feature.go") {
		t.Errorf("FilesTouched = %v, want to contain feature.go", res.FilesTouched)
	}
	sum, err := jr.Read(res.JournalSeq)
	if err != nil {
		t.Fatalf("journal.Read: %v", err)
	}
	if !contains(sum.Files, "feature.go") {
		t.Errorf("journal Files = %v, want to contain feature.go", sum.Files)
	}
}

// TestIterateNonRepoNoGitSignal: a non-repo target yields no git signal — work is
// reported false and no files touched, but reconciliation (test gate) still runs.
func TestIterateNonRepoNoGitSignal(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = stubSuccess

	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.WorkHappened || res.FilesTouched != nil {
		t.Errorf("non-repo: WorkHappened=%v Files=%v, want false/nil", res.WorkHappened, res.FilesTouched)
	}
}

// TestReadRulesFallsBackToDefault: with no .flanders/rules.md on disk, the loop must
// still apply the built-in loop discipline (spec 01 §invocation) — the rules are
// never silently empty just because the project skipped `flanders init`.
func TestReadRulesFallsBackToDefault(t *testing.T) {
	cfg, p, jr := setupProject(t)
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, statErr := os.Stat(p.Rules); !os.IsNotExist(statErr) {
		t.Fatalf("precondition: rules file should not exist, stat err = %v", statErr)
	}
	got, err := d.readRules()
	if err != nil {
		t.Fatalf("readRules: %v", err)
	}
	if got != rules.DefaultMarkdown {
		t.Errorf("readRules with no file = %q, want the built-in default", got)
	}
}

// TestReadRulesReadsFile: when the project has a rules file (e.g. a user's tuned copy
// or the one `flanders init` wrote), the loop uses that file's contents verbatim.
func TestReadRulesReadsFile(t *testing.T) {
	cfg, p, jr := setupProject(t)
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	custom := "# project rules\n\nAlways do X.\n"
	if err := os.WriteFile(p.Rules, []byte(custom), 0o644); err != nil {
		t.Fatalf("write rules: %v", err)
	}
	got, err := d.readRules()
	if err != nil {
		t.Fatalf("readRules: %v", err)
	}
	if got != custom {
		t.Errorf("readRules = %q, want the file's contents %q", got, custom)
	}
}

// newComposeDriver builds a Driver over a temp project so the compose tests can call
// the method form of composePrompt (which resolves spec references against
// d.paths.Root). It returns the driver and the project root for tests that write
// referenced spec files.
func newComposeDriver(t *testing.T) (*Driver, string) {
	t.Helper()
	cfg, p, jr := setupProject(t)
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d, p.Root
}

// TestComposePromptInjectsTaskFileAndSummary: the prompt embeds the current task file
// (frontmatter + body) so the agent sees its acceptance criterion and deps, plus the
// one-line plan summary.
func TestComposePromptInjectsTaskFileAndSummary(t *testing.T) {
	d, _ := newComposeDriver(t)
	store, err := task.NewStore([]*task.Task{
		mkTask("0001", task.StatusDone, nil),
		mkTask("0007", task.StatusPending, []string{"0001"}),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	prompt, err := d.composePrompt(store.ByID("0007"), store, "build")
	if err != nil {
		t.Fatalf("composePrompt: %v", err)
	}
	for _, want := range []string{"Current task: 0007", "acceptance:", "go test ./... passes for 0007", "## Task 0007", "1/2 tasks done"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

// TestComposePromptIncludesDependencyOutcomes: a dependency's OUTCOME (its id, status,
// and acceptance criterion it met) is injected so the loop builds on it — but only the
// summary, never the dependency's full body (that would defeat the cost lever).
func TestComposePromptIncludesDependencyOutcomes(t *testing.T) {
	d, _ := newComposeDriver(t)
	store, err := task.NewStore([]*task.Task{
		mkTask("0001", task.StatusDone, nil),
		mkTask("0007", task.StatusPending, []string{"0001"}),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	prompt, err := d.composePrompt(store.ByID("0007"), store, "build")
	if err != nil {
		t.Fatalf("composePrompt: %v", err)
	}
	for _, want := range []string{"## Dependency outcomes", "**0001** (done)", "go test ./... passes for 0001"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing dependency outcome %q:\n%s", want, prompt)
		}
	}
	// The dep's BODY heading must not leak — only its frontmatter-derived summary.
	if strings.Contains(prompt, "## Task 0001") {
		t.Errorf("prompt leaked the dependency's body, not just its summary:\n%s", prompt)
	}
}

// TestComposePromptExcludesUnrelatedTasks: a task that is neither the current task nor
// one of its dependencies contributes nothing to the prompt — the core cost lever
// (spec 01: never the whole plan). Here 0002 is unrelated to the current 0007.
func TestComposePromptExcludesUnrelatedTasks(t *testing.T) {
	d, _ := newComposeDriver(t)
	store, err := task.NewStore([]*task.Task{
		mkTask("0001", task.StatusDone, nil),
		mkTask("0002", task.StatusDone, nil), // unrelated: not a dep of 0007
		mkTask("0007", task.StatusPending, []string{"0001"}),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	prompt, err := d.composePrompt(store.ByID("0007"), store, "build")
	if err != nil {
		t.Fatalf("composePrompt: %v", err)
	}
	for _, leak := range []string{"go test ./... passes for 0002", "## Task 0002", "**0002**"} {
		if strings.Contains(prompt, leak) {
			t.Errorf("prompt leaked unrelated task content %q:\n%s", leak, prompt)
		}
	}
}

// TestComposePromptInjectsReferencedSpecExcerpts: the prompt includes ONLY the spec
// section a task names via `specs/x.md §Section` (spec 01: "only the named spec
// excerpts the task references") — the referenced section's content is present and an
// unreferenced sibling section is excluded.
func TestComposePromptInjectsReferencedSpecExcerpts(t *testing.T) {
	d, root := newComposeDriver(t)
	specPath := filepath.Join(root, "specs", "sample.md")
	specBody := "# Sample spec\n\n" +
		"## Wanted section\n\nThis is the WANTED content the task references.\n\n" +
		"### Wanted detail\n\nNested detail still inside Wanted.\n\n" +
		"## Other section\n\nThis UNWANTED content must not appear.\n"
	if err := os.WriteFile(specPath, []byte(specBody), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	body := "## Do the work\n\nReferences: specs/sample.md §Wanted section.\n"
	target := task.New("0001", task.StatusPending, nil, "acceptance for 0001", body)
	store, err := task.NewStore([]*task.Task{target})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	prompt, err := d.composePrompt(store.ByID("0001"), store, "build")
	if err != nil {
		t.Fatalf("composePrompt: %v", err)
	}
	for _, want := range []string{"## Referenced spec excerpts", "## Wanted section", "WANTED content", "Nested detail still inside Wanted"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing referenced excerpt %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "UNWANTED") || strings.Contains(prompt, "Other section") {
		t.Errorf("prompt leaked an unreferenced spec section:\n%s", prompt)
	}
}

// TestExtractSection locks the subtle parts of section extraction: a heading inside a
// fenced code block is not a boundary, a parenthetical heading is matched by its bare
// name (the prefix rule), and the section runs to the next same-or-higher heading.
func TestExtractSection(t *testing.T) {
	const md = "# Title\n\n" +
		"## Prompt composition (the cost/quality lever)\n\n" +
		"Body line.\n\n" +
		"```sh\n# not a heading\n## also not a heading\n```\n\n" +
		"More body.\n\n" +
		"### Subsection\n\nstill inside.\n\n" +
		"## Next section\n\nexcluded.\n"

	got, ok := extractSection(md, "Prompt composition")
	if !ok {
		t.Fatal("extractSection: section not found by bare name")
	}
	for _, want := range []string{"Prompt composition (the cost/quality lever)", "Body line.", "# not a heading", "More body.", "### Subsection", "still inside."} {
		if !strings.Contains(got, want) {
			t.Errorf("excerpt missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Next section") || strings.Contains(got, "excluded.") {
		t.Errorf("excerpt ran past its boundary:\n%s", got)
	}

	if _, ok := extractSection(md, "Nonexistent"); ok {
		t.Error("extractSection matched a nonexistent section")
	}
}

// TestComposePromptSkipsUnresolvableSpecRefs: a reference to a missing file or a
// missing section is a spec-authoring slip, not a harness fault — it is silently
// skipped (no excerpt section emitted) and composition still succeeds.
func TestComposePromptSkipsUnresolvableSpecRefs(t *testing.T) {
	d, root := newComposeDriver(t)
	// A real file, but the referenced section does not exist in it.
	if err := os.WriteFile(filepath.Join(root, "specs", "real.md"), []byte("# Real\n\n## Present\n\nhi\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	body := "References: specs/missing.md §Whatever and specs/real.md §Absent section.\n"
	target := task.New("0001", task.StatusPending, nil, "acceptance", body)
	store, err := task.NewStore([]*task.Task{target})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	prompt, err := d.composePrompt(store.ByID("0001"), store, "build")
	if err != nil {
		t.Fatalf("composePrompt: %v", err)
	}
	if strings.Contains(prompt, "## Referenced spec excerpts") {
		t.Errorf("expected no excerpt section for unresolvable refs:\n%s", prompt)
	}
}
