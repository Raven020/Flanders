package loop

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"flanders/src/lib/git"
	"flanders/src/lib/invoke"
	"flanders/src/lib/stream"
	"flanders/src/lib/supervise"
	"flanders/src/lib/task"
)

// PlanIterate runs one plan-phase loop (plan task 4.2). The plan phase reads the
// user-authored specs (specs/*.md) and emits/updates the task files
// (specs/tasks/*.md): decomposing requirements into smallest-checkable tasks,
// assigning ids, wiring deps, writing acceptance criteria (spec 02 §Plan lifecycle).
//
// Why this is a separate method, not a phase branch inside Iterate. Iterate's spine
// is task-selected: it calls store.Next(), composes around that one task file, runs
// the test gate, and reconciles that task's status. A plan loop has none of those —
// it is NOT selected (it always operates on the whole spec set), produces task files
// rather than code (so the test gate is meaningless and runsTestGate already excludes
// "plan"), and has no single task whose status to reconcile. Forcing it through
// Iterate would mean branching out select/compose/verify/evaluate — the majority of
// that method. So the genuinely shared half — spawn, observe, journal, checkpoint, and
// the context-pressure guard — is factored into helpers both call (d.commit,
// d.baseSummary, newContextGuard), and the two loop shapes stay readable on their own.
//
// Store hot-reload still holds: composePlanPrompt re-reads specs/tasks/*.md every call,
// so a task file written by the previous plan loop is visible to the next one (the
// agent extends rather than duplicates). iter is the orchestrator's per-phase counter,
// used only for the checkpoint message's {iter} variable.
//
// A returned error is an infrastructure/config failure (no specs to plan from, couldn't
// build the command, or couldn't spawn) — the orchestrator surfaces and halts. A loop
// that ran but produced an error/limit/timeout *result* is not an error: it completes
// with Result.Outcome set, the same contract as Iterate. The returned Result reuses the
// loop Result struct; its task-specific fields (Task, Verify, Reconcile, AllDone) stay
// zero for a plan loop — the orchestrator reads Phase=="plan" and judges
// plan-completeness separately (plan task 4.3).
func (d *Driver) PlanIterate(ctx context.Context, iter int) (*Result, error) {
	const phase = "plan"
	start := d.now()

	// 1. COMPOSE — the plan prompt: the decompose instruction + a summary of the task
	// files already on disk (hot-reloaded every loop) + the non-task specs verbatim.
	prompt, err := d.composePlanPrompt()
	if err != nil {
		return nil, fmt.Errorf("loop: compose plan prompt: %w", err)
	}
	rules, err := d.readRules()
	if err != nil {
		return nil, fmt.Errorf("loop: read rules: %w", err)
	}

	// 2. SPAWN — mint a fresh session id, build the plan invocation (opus/high by
	// default, per config.PhaseClass("plan")), and supervise it. The raw transcript is
	// teed into buf for the journal archive.
	sid, err := d.newSessionID()
	if err != nil {
		return nil, fmt.Errorf("loop: %w", err)
	}
	cmd, err := invoke.Build(d.cfg, invoke.Spec{
		Phase:              phase,
		SessionID:          sid,
		SystemPromptAppend: rules,
		Prompt:             prompt,
	})
	if err != nil {
		return nil, fmt.Errorf("loop: build plan invocation: %w", err)
	}
	// Snapshot the working tree before the agent runs so the after-loop diff reveals the
	// task files THIS loop wrote — the plan loop's progress signal (no status flip to
	// read, unlike a build loop). Best-effort: a non-repo target yields no signal.
	pre := git.Snap(ctx, d.paths.Root)

	// The context-pressure guard applies to a plan loop too — a decomposition that runs
	// too deep degrades like any other (spec 01 §context-pressure). The tier-2 wind-down
	// here is planWindDownMessage: there is no single task to self-block, so it steers the
	// agent to commit the task files it has written and end (the next plan loop continues
	// with fresh context). A tier-3 hard kill just ends the loop and is named in the
	// journal; unlike a build loop there is no task to mark blocked, so the driver does
	// NOT call markContextOverreach here.
	guard := newContextGuard(d.cfg, d.log, planWindDownMessage)
	var raw bytes.Buffer
	res, err := d.run(ctx, supervise.Spec{
		Command:     cmd,
		Prompt:      prompt,
		StreamInput: d.cfg.Agent.StreamInput,
		Timeout:     d.cfg.Guardrails.IterationTimeout.Duration,
		RawSink:     &raw,
		OnEvent:     func(p *supervise.Proc, ev *stream.Event) { guard.handle(p, ev) },
		Log:         d.log,
	})
	if err != nil {
		return nil, fmt.Errorf("loop: spawn plan agent: %w", err)
	}

	// 3. OBSERVE — classify the outcome and record the loop in the journal. No verify
	// step (plan produces task files, not code — runsTestGate excludes "plan") and no
	// reconcile step (no single selected task whose status to reconcile).
	obs := res.Observation
	if obs == nil {
		obs = &stream.LoopObservation{} // supervise guarantees non-nil; belt-and-braces
	}
	outcome := obs.Classify(res.ExitCode)
	workHappened, files := git.Diff(pre, git.Snap(ctx, d.paths.Root))

	class, _ := d.cfg.PhaseClass(phase) // phase already validated by invoke.Build above
	// A plan loop's journal record is the phase-agnostic base only: Task stays "" (spec:
	// "empty for plan/discuss"), no test gate ran, and there is no single task status to
	// record. Files lists the task files the loop authored, from the git diff.
	sum := d.baseSummary(phase, sid, class, res, obs, start, outcome, files)
	if guard.hardKilled() {
		// A hard kill reads as a generic process error to Classify (killed mid-stream),
		// so name the backstop precisely in the history.
		sum.Error = "context-pressure hard backstop (≥ context.hard_pct): harness ended the plan loop"
	}
	seq, err := d.journal.Append(sum, &raw)
	if err != nil {
		return nil, fmt.Errorf("loop: journal plan loop: %w", err)
	}

	// 4. CHECKPOINT — on progress (the plan loop wrote/changed task files), commit so the
	// iteration is revertable. "Progress" for plan is a working-tree change (workHappened),
	// not a status flip; planCheckpoint passes it through to the shared commit core.
	checkpointSHA := d.planCheckpoint(ctx, phase, iter, workHappened)

	d.log.Info("plan iteration complete",
		"phase", phase, "session", sid,
		"outcome", outcome.String(), "exit", res.ExitCode,
		"timed_out", res.TimedOut, "journal_seq", seq,
		"work_happened", workHappened, "files", len(files),
		"checkpoint", checkpointSHA,
		"context_trip", guard.peakTrip().String(), "cost_usd", obs.Cost)

	return &Result{
		Phase:        phase,
		SessionID:    sid,
		Observation:  obs,
		Outcome:      outcome,
		ExitCode:     res.ExitCode,
		TimedOut:     res.TimedOut,
		JournalSeq:   seq,
		Duration:     d.now().Sub(start),
		WorkHappened: workHappened,
		FilesTouched: files,
		Checkpoint:   checkpointSHA,
		ContextTrip:  guard.peakTrip(),
	}, nil
}

// composePlanPrompt builds the plan-loop prompt (plan task 4.2, spec 02 §Plan lifecycle).
// Unlike the per-task composePrompt — whose whole point is to inject ONLY the current
// task (the cost lever for the build loop) — the plan agent's input genuinely is the
// whole spec set: there is no smaller unit to decompose from. So this injects, in order:
//
//  1. the decompose-to-smallest-checkable instruction (the plan agent's contract),
//  2. a compact summary of the task files already on disk (so it extends rather than
//     duplicates and picks fresh, non-colliding ids — store hot-reloaded every loop),
//  3. the non-task specs (specs/*.md) verbatim — the material to decompose.
//
// No spec files is an infrastructure/config error, not an empty plan: there is nothing
// to plan from, so the caller surfaces and halts rather than spawning an agent with
// nothing to do.
func (d *Driver) composePlanPrompt() (string, error) {
	specs, err := d.readPlanSpecs()
	if err != nil {
		return "", err
	}
	if len(specs) == 0 {
		return "", fmt.Errorf("no spec files (*.md) found under %s — nothing to plan from", d.paths.Specs)
	}
	// Existing task files (hot-reload): load and summarize so the agent sees the current
	// plan state and the id space already in use.
	store, err := task.LoadDir(d.paths.Tasks)
	if err != nil {
		return "", fmt.Errorf("load existing tasks: %w", err)
	}

	var b strings.Builder
	b.WriteString(planInstructions)
	b.WriteString(existingTasksSummary(store))
	b.WriteString("## Specifications\n\n")
	b.WriteString("These are the user-authored specs. Decompose EVERY requirement below into ")
	b.WriteString("one or more task files. Reference the spec section each task implements in its body.\n\n")
	for _, s := range specs {
		fmt.Fprintf(&b, "### %s\n\n```markdown\n", s.name)
		b.WriteString(s.content)
		if !strings.HasSuffix(s.content, "\n") {
			b.WriteByte('\n')
		}
		b.WriteString("```\n\n")
	}
	return b.String(), nil
}

// planInstructions is the plan agent's standing contract — the plan-phase analogue of a
// task's "work this one task" header. It is appended ahead of the specs so the agent
// knows its job before it reads the material: author smallest-checkable task files that
// cover every requirement, and nothing else.
const planInstructions = `# Plan phase — decompose the specs into task files

You are the PLAN agent. Your only job this loop is to read the specifications below and
write/update task files under ` + "`specs/tasks/`" + `. You do NOT implement anything and you
do NOT edit the specs — you only author task files.

Rules:

- A task is the SMALLEST change with a checkable acceptance criterion — roughly one test
  going green, completable by a fresh-context agent in a single pass. Too coarse and a
  build loop cannot finish it; too fine and per-loop overhead dominates.
- Each task file is YAML frontmatter + a markdown body, named ` + "`<id>-<slug>.md`" + ` where
  the zero-padded numeric id matches the filename prefix (e.g. ` + "`0007-loop-engine.md`" + `
  has ` + "`id: 0007`" + `). Frontmatter fields:
    - ` + "`id`" + ` — stable numeric id (matches the filename prefix).
    - ` + "`status: pending`" + ` — new tasks start pending.
    - ` + "`deps: [ids]`" + ` — task ids that must be ` + "`done`" + ` first (the harness never selects
      a task before its deps are done; wire prerequisites, e.g. a parser before the
      engine that uses it).
    - ` + "`acceptance`" + ` — the single checkable criterion (it feeds the harness test gate).
  The body is a short description; reference the spec section(s) the task implements as
  ` + "`specs/NN-name.md §Section`" + ` so the build loop can inject just those excerpts.
- COVER EVERY REQUIREMENT: every requirement in the specs must map to at least one task.
  Residual gaps are acceptable — they surface later during build as blocked tasks and get
  batch-corrected — but aim for full coverage now.
- Extend/refine the existing tasks (listed below) rather than duplicating them; assign
  fresh, non-colliding ids for new tasks.
- Delegate reading large specs to subagents to keep your own context lean.
- Do not hand-edit harness-owned state (` + "`.flanders/`" + `, ` + "`IMPLEMENTATION_PLAN.md`" + `).

`

// planWindDownMessage is the tier-2 soft wind-down the context guard injects at soft_pct
// during a PLAN loop. The task-loop windDownMessage tells the agent to self-block "this
// task" — but a plan loop has no single task, so this variant instead steers the agent
// to finish and commit the task files it has authored and end the session, leaving the
// rest of the decomposition for the next plan loop's fresh context.
const planWindDownMessage = "⚠ CONTEXT-PRESSURE WIND-DOWN (from the Flanders harness). You are past " +
	"~75% of the context window, where quality degrades and auto-compaction risks mangling your work. " +
	"Stop starting new task files now and wrap up THIS plan loop gracefully: (1) finish writing any task " +
	"file you have already started, making sure its YAML frontmatter is valid; (2) commit your work; " +
	"(3) end the session. The next plan loop will continue the decomposition with fresh context."

// planCheckpoint commits the task files a plan loop wrote (step 4, spec 01 §Checkpointing).
// It defers to the shared commit core; the only plan-specific inputs are the progress
// signal (a working-tree change, since there is no status flip to read) and the
// {task}/{result} message variables. {result} reports the task-file count now on disk —
// the most useful one-glance plan-loop outcome for a commit log.
func (d *Driver) planCheckpoint(ctx context.Context, phase string, iter int, workHappened bool) string {
	count := 0
	if store, err := task.LoadDir(d.paths.Tasks); err == nil {
		count = len(store.Tasks())
	}
	return d.commit(ctx, phase, iter, "(plan)", fmt.Sprintf("%d tasks", count), workHappened)
}

// planSpec is one non-task spec file read for the plan prompt.
type planSpec struct {
	name    string // base name, e.g. "01-ralph-loop.md"
	content string
}

// readPlanSpecs reads the non-task spec files (specs/*.md) the plan loop decomposes. The
// glob is non-recursive, so the generated task files under specs/tasks/ are never pulled
// in; the explicit tasks-dir guard additionally covers a degenerate config where the
// specs and tasks dirs coincide. Sub-directories that happen to match *.md and any file
// that cannot be read as a regular file are skipped. Returns the files sorted by name so
// the prompt (and tests) are deterministic.
func (d *Driver) readPlanSpecs() ([]planSpec, error) {
	matches, err := filepath.Glob(filepath.Join(d.paths.Specs, "*.md"))
	if err != nil {
		return nil, fmt.Errorf("glob specs %q: %w", d.paths.Specs, err)
	}
	sort.Strings(matches)
	tasksDir := filepath.Clean(d.paths.Tasks)
	out := make([]planSpec, 0, len(matches))
	for _, m := range matches {
		if filepath.Clean(filepath.Dir(m)) == tasksDir {
			continue // a degenerate specs==tasks layout; never treat task files as input
		}
		info, err := os.Stat(m)
		if err != nil || info.IsDir() {
			continue // a directory named *.md, or a vanished file — not a spec
		}
		data, err := os.ReadFile(m)
		if err != nil {
			return nil, fmt.Errorf("read spec %q: %w", m, err)
		}
		out = append(out, planSpec{name: filepath.Base(m), content: string(data)})
	}
	return out, nil
}

// existingTasksSummary renders a compact, frontmatter-derived view of the task files
// already on disk — item 2 of the plan prompt. Like dependencyOutcomes it injects only
// id/status/deps/acceptance, never the task bodies: the agent needs to know what exists
// and which ids are taken, not re-read every task in full. The first-plan-loop case
// (none yet) is stated explicitly so the agent knows to start ids at 0001.
func existingTasksSummary(store *task.Store) string {
	tasks := store.Tasks()
	if len(tasks) == 0 {
		return "## Existing tasks\n\nNone yet — this is the first plan loop. Start ids at 0001.\n\n"
	}
	var b strings.Builder
	b.WriteString("## Existing tasks\n\n")
	b.WriteString("These task files already exist under specs/tasks/. Refine or extend them and assign ")
	b.WriteString("fresh non-colliding ids for new tasks; do not duplicate a requirement already covered:\n\n")
	for _, t := range tasks {
		fmt.Fprintf(&b, "- **%s** (%s)", t.ID(), t.Status())
		if deps := t.Deps(); len(deps) > 0 {
			fmt.Fprintf(&b, " deps=[%s]", strings.Join(deps, ", "))
		}
		if acc := strings.TrimSpace(t.Acceptance()); acc != "" {
			fmt.Fprintf(&b, " — %s", oneLine(acc))
		}
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.String()
}
