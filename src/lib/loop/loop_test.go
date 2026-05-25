package loop

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flanders/src/lib/config"
	"flanders/src/lib/invoke"
	"flanders/src/lib/journal"
	"flanders/src/lib/paths"
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

	res, err := d.Iterate(context.Background(), "build")
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

	res, err := d.Iterate(context.Background(), "build")
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
	res, err := d.Iterate(context.Background(), "build")
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
	res, err := d.Iterate(context.Background(), "build")
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
	if _, err := d.Iterate(context.Background(), "build"); err == nil {
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

	res, err := d.Iterate(context.Background(), "build")
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

// TestComposePromptInjectsTaskFileVerbatim: the prompt embeds the task file
// (frontmatter + body) so the agent sees its acceptance criterion and deps, plus
// the one-line plan summary — and nothing about unrelated tasks' bodies.
func TestComposePromptInjectsTaskFileVerbatim(t *testing.T) {
	store, err := task.NewStore([]*task.Task{
		mkTask("0001", task.StatusDone, nil),
		mkTask("0007", task.StatusPending, []string{"0001"}),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	target := store.ByID("0007")
	prompt, err := composePrompt(target, store)
	if err != nil {
		t.Fatalf("composePrompt: %v", err)
	}
	for _, want := range []string{"Current task: 0007", "acceptance:", "go test ./... passes for 0007", "1/2 tasks done"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Task 0001") {
		t.Errorf("prompt leaked an unrelated task's body:\n%s", prompt)
	}
}
