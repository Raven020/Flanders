package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flanders/src/lib/git"
	"flanders/src/lib/paths"
	"flanders/src/lib/stream"
	"flanders/src/lib/supervise"
	"flanders/src/lib/task"
)

// writeSpec drops a non-task spec file under the project's specs/ dir — the planning
// input the plan loop decomposes. (setupProject already creates specs/ as the parent of
// specs/tasks/.)
func writeSpec(t *testing.T, p *paths.Paths, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(p.Specs, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write spec %s: %v", name, err)
	}
}

// planTaskFile is a minimal VALID task file (parses + Validate()s) a plan-loop stub can
// write, so a subsequent task.LoadDir (e.g. planCheckpoint's count) sees one task.
const planTaskFile = `---
id: 0001
status: pending
acceptance: "go test ./feature passes"
---
## Do the thing

Implements specs/01-feature.md §Requirement.
`

// TestComposePlanPromptInjectsSpecsAndInstruction: the plan prompt embeds the decompose
// instruction (the plan agent's contract) and the spec content verbatim, and — on the
// first plan loop, with no tasks yet — tells the agent to start ids at 0001.
func TestComposePlanPromptInjectsSpecsAndInstruction(t *testing.T) {
	cfg, p, jr := setupProject(t) // no tasks
	writeSpec(t, p, "01-feature.md", "# Feature\n\n## Requirement\n\nDo the THING precisely.\n")
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	prompt, err := d.composePlanPrompt()
	if err != nil {
		t.Fatalf("composePlanPrompt: %v", err)
	}
	// The decompose instruction and its key obligations are present.
	for _, want := range []string{"Plan phase", "SMALLEST change", "specs/tasks/", "COVER EVERY REQUIREMENT"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing instruction text %q:\n%s", want, prompt)
		}
	}
	// The spec content is injected verbatim, labelled by file name.
	for _, want := range []string{"## Specifications", "### 01-feature.md", "Do the THING precisely."} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing spec content %q:\n%s", want, prompt)
		}
	}
	// First plan loop: no existing tasks → the start-at-0001 note.
	if !strings.Contains(prompt, "Start ids at 0001") {
		t.Errorf("prompt missing first-loop existing-tasks note:\n%s", prompt)
	}
}

// TestComposePlanPromptSummarizesExistingTasks: when task files already exist (a later
// plan loop, or a re-plan), the prompt lists each by id/status/acceptance so the agent
// extends rather than duplicates them — but NEVER their full bodies (the cost lever, as
// in dependencyOutcomes). And task files are never pulled in as "specs".
func TestComposePlanPromptSummarizesExistingTasks(t *testing.T) {
	cfg, p, jr := setupProject(t,
		task.New("0001", task.StatusDone, nil, "parser acceptance criterion", "## Parser\n\nFULL BODY must not leak into the plan prompt.\n"),
	)
	writeSpec(t, p, "01-feature.md", "# Feature\n\n## Requirement\n\nDo the thing.\n")
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	prompt, err := d.composePlanPrompt()
	if err != nil {
		t.Fatalf("composePlanPrompt: %v", err)
	}
	for _, want := range []string{"## Existing tasks", "**0001** (done)", "parser acceptance criterion"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing existing-task summary %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "FULL BODY must not leak") {
		t.Errorf("prompt leaked an existing task's body, not just its summary:\n%s", prompt)
	}
	// The spec is still injected; the task file is not mistaken for a spec.
	if !strings.Contains(prompt, "Do the thing.") {
		t.Errorf("prompt missing spec content:\n%s", prompt)
	}
}

// TestComposePlanPromptNoSpecsErrors: a plan loop with no spec files has nothing to plan
// from — an infrastructure/config error the orchestrator surfaces, not an empty run.
func TestComposePlanPromptNoSpecsErrors(t *testing.T) {
	cfg, p, jr := setupProject(t) // creates specs/ and specs/tasks/, but no *.md under specs/
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := d.composePlanPrompt(); err == nil || !strings.Contains(err.Error(), "nothing to plan from") {
		t.Fatalf("composePlanPrompt with no specs: err = %v, want a 'nothing to plan from' error", err)
	}
}

// TestPlanIterateSpawnsAndJournals is the headline 4.2 acceptance (harness side): a plan
// loop composes the plan prompt, spawns a fresh plan-class (opus/high) session, and
// writes a journal entry with phase=plan, no task id, and no test gate run.
func TestPlanIterateSpawnsAndJournals(t *testing.T) {
	cfg, p, jr := setupProject(t)
	writeSpec(t, p, "01-feature.md", "# Feature\n\n## Requirement\n\nDo the thing.\n")
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.newSessionID = func() (string, error) { return "plan-sess", nil }

	var gotSpec supervise.Spec
	d.run = func(_ context.Context, spec supervise.Spec) (*supervise.Result, error) {
		gotSpec = spec
		if spec.RawSink != nil {
			_, _ = spec.RawSink.Write([]byte(`{"type":"result","subtype":"success"}` + "\n"))
		}
		return &supervise.Result{
			Observation: &stream.LoopObservation{
				Done: true, Subtype: "success", Cost: 0.03,
				FinalUsage: stream.Usage{InputTokens: 500, OutputTokens: 120},
				Subagents:  []stream.SubagentSpawn{{SubagentType: "Explore"}},
			},
			ExitCode: 0,
		}, nil
	}

	res, err := d.PlanIterate(context.Background(), 1)
	if err != nil {
		t.Fatalf("PlanIterate: %v", err)
	}
	if res.Phase != "plan" || res.Task != nil || res.NoWork {
		t.Errorf("Result identity wrong: phase=%q task=%v noWork=%v, want plan/nil/false", res.Phase, res.Task, res.NoWork)
	}
	if res.Outcome != stream.OutcomeSuccess || res.SessionID != "plan-sess" || res.JournalSeq == 0 {
		t.Errorf("Result = outcome %v session %q seq %d, want success/plan-sess/non-zero", res.Outcome, res.SessionID, res.JournalSeq)
	}

	// Compose: the prompt names the plan job and embeds the spec; never an empty turn.
	if !strings.Contains(gotSpec.Prompt, "Plan phase") || !strings.Contains(gotSpec.Prompt, "Do the thing.") {
		t.Errorf("plan prompt missing instruction or spec content:\n%s", gotSpec.Prompt)
	}
	// Spawn: a fresh-session invocation built for the plan phase.
	if argv := strings.Join(gotSpec.Command.Argv(), " "); gotSpec.Command.Bin != "claude" || !strings.Contains(argv, "--session-id plan-sess") {
		t.Errorf("invocation wrong: bin=%q argv=%s", gotSpec.Command.Bin, argv)
	}

	// Journal: phase=plan, no task id, plan class (opus/high), and the test gate did NOT run.
	sum, err := jr.Read(res.JournalSeq)
	if err != nil {
		t.Fatalf("journal.Read: %v", err)
	}
	if sum.Phase != "plan" || sum.Task != "" || sum.SessionID != "plan-sess" {
		t.Errorf("summary identity wrong: phase=%q task=%q session=%q", sum.Phase, sum.Task, sum.SessionID)
	}
	if sum.Model != "opus" || sum.Effort != "high" {
		t.Errorf("summary class = %q/%q, want opus/high (plan phase default)", sum.Model, sum.Effort)
	}
	if sum.Cost != 0.03 || sum.Tokens.Input != 500 || sum.Tokens.Output != 120 {
		t.Errorf("summary cost/tokens wrong: cost=%v tokens=%+v", sum.Cost, sum.Tokens)
	}
	if len(sum.Subagents) != 1 || sum.Subagents[0].Name != "Explore" {
		t.Errorf("subagents = %+v, want one Explore", sum.Subagents)
	}
	if sum.Test.Ran {
		t.Error("Test.Ran = true, want false (a plan loop produces task files, not code — no test gate)")
	}
	if sum.StatusBefore != "" || sum.StatusAfter != "" {
		t.Errorf("status transition = %q→%q, want empty (no single task)", sum.StatusBefore, sum.StatusAfter)
	}
}

// TestPlanIterateCheckpointsTaskFiles: a plan loop's "progress" is the task files it
// writes (there is no status flip to read). When the agent authors a task file in a git
// repo, the harness commits it with the plan-templated message and surfaces the sha.
func TestPlanIterateCheckpointsTaskFiles(t *testing.T) {
	cfg, p, jr := setupProject(t)
	cfg.Git.Enabled = true // re-enable (setupProject disables it)
	writeSpec(t, p, "01-feature.md", "# Feature\n\n## Requirement\n\nDo the thing.\n")
	initGitRepo(t, p.Root) // clean baseline: commits the spec
	base := git.HeadSHA(context.Background(), p.Root)

	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = func(_ context.Context, _ supervise.Spec) (*supervise.Result, error) {
		// The plan agent writes a task file decomposing the requirement.
		if err := os.WriteFile(filepath.Join(p.Tasks, "0001-do-thing.md"), []byte(planTaskFile), 0o644); err != nil {
			t.Fatalf("plan agent write task: %v", err)
		}
		return &supervise.Result{Observation: &stream.LoopObservation{Done: true, Subtype: "success"}, ExitCode: 0}, nil
	}

	res, err := d.PlanIterate(context.Background(), 2)
	if err != nil {
		t.Fatalf("PlanIterate: %v", err)
	}
	if !res.WorkHappened {
		t.Error("WorkHappened = false, want true (a task file was authored)")
	}
	if !contains(res.FilesTouched, "specs/tasks/0001-do-thing.md") {
		t.Errorf("FilesTouched = %v, want to contain the new task file", res.FilesTouched)
	}
	if res.Checkpoint == "" {
		t.Fatal("Result.Checkpoint = \"\", want a commit sha (the plan loop made progress)")
	}
	if now := git.HeadSHA(context.Background(), p.Root); now == base || now != res.Checkpoint {
		t.Errorf("HEAD = %q (base %q), Result.Checkpoint = %q; want HEAD advanced to the checkpoint", now, base, res.Checkpoint)
	}
	// The message uses the plan-loop {task}/{result} variables: a placeholder task name
	// and the resulting task-file count.
	if got, want := headSubject(t, p.Root), "Flanders: plan #2 — (plan) [1 tasks]"; got != want {
		t.Errorf("commit subject = %q, want %q", got, want)
	}
}
