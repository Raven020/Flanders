// Package loop is the Ralph iteration driver — the engine that runs ONE loop of
// the iteration anatomy (specs/01-ralph-loop.md §Iteration anatomy):
//
//  1. select  — hot-reload the task store from disk, pick the next actionable task
//  2. compose — build the prompt for that task (+ loop rules via system prompt)
//  3. spawn   — invoke a fresh `claude -p` session and supervise it
//  4. observe — fold the stream-json into a LoopObservation, classify the outcome
//  5. verify  — run the harness-owned ground-truth test gate (spec 01 §verify)
//  6. evaluate— reconcile the task's status (spec 02 §Mutation ownership) and
//     record the git "did work happen" signal (+ archive the raw transcript and a
//     summary, incl. the test result and reconciled status, to the journal)
//  7. checkpoint — on progress (a status change or a passing test gate), commit the
//     working tree so the iteration is revertable (spec 01 §Checkpointing)
//
// Why a driver that runs exactly one iteration, not a loop. Ralph's defining rule
// is "fresh context every iteration; durable state on disk" (spec 01 §Principle).
// Each call to [Driver.Iterate] is a clean, self-contained pass: it reads the
// current truth off disk, runs one unit of work, records the result, and returns.
// The orchestrator (Phase 5) is what calls Iterate repeatedly and owns the
// run-state machine (iteration counts, stall, usage waits, phase transitions);
// keeping that out of here means the driver has one job and is trivially testable.
//
// Why select re-reads the store every iteration. An in-loop split — an agent
// writing new task files mid-run while it has context to spare (spec 06
// §Refinement) — must be visible to the very next iteration. So the store is
// rebuilt from specs/tasks/*.md at the TOP of every Iterate, never cached across
// loops. The files on disk are the single source of truth (spec 02 §Storage).
//
// How status mutation is split. Spec 02 §Mutation ownership locks that the agent
// flips its task's `status` (`active` while working, `done`/`blocked` on exit) and
// the harness cross-checks via the signals it owns. The driver re-reads the task
// after the loop and runs the evaluate step through src/lib/reconcile: it respects
// an agent-written terminal status, otherwise promotes to `done` when the test gate
// passed, otherwise normalizes a stuck `active` back to `pending` (see that package
// for the policy). So the outcome is recorded whether the agent flipped it or not.
//
// The verify step (5) runs the harness-owned test gate (src/lib/verify) — the
// ground truth for "done" (spec 01 §done-detection: the canonical test command's
// exit code, not the agent's self-report). It runs only for the code-producing
// phases (build/test) on a cleanly-completed invocation, and its test result is
// recorded in the journal and surfaced on Result for the orchestrator's
// done-detection (plan task 3.7). The checkpoint step (7, checkpoint.go) commits the
// loop's work on progress per [git].commit_each (spec 01 §Checkpointing) and surfaces
// the new commit sha on Result for the orchestrator to persist as state.last_checkpoint
// (spec 09); it does NOT touch state.json itself. The context-pressure guardrail
// (context_pressure.go, plan task 3.11) is wired into the spawn/observe steps: a per-loop
// guard drives a live occupancy tracker off the supervisor's OnEvent hook and, on a trip,
// injects a graceful wind-down (~soft_pct, when stream_input is on) or hard-kills and
// writes `blocked: context-overreach` itself (~hard_pct, the harness owning the marker).
// What this package still defers (each its own plan item, wired into the same Iterate
// spine when it lands): the per-iteration-timeout and usage-limit guardrails (3.10, 3.12),
// and the stall/max-iteration accounting the orchestrator owns (3.8–3.9). The prompt composition
// (compose.go) is the full cost/quality lever (plan task 3.2): the current task file
// + the outcomes of its dependencies + only the spec excerpts it references + a
// one-line plan summary, with the loop rules appended as a system prompt. None of
// the deferred items are stubbed — they are simply not yet steps of the iteration;
// the driver returns the observation and selected task so the orchestrator has what
// the later steps need.
package loop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"flanders/src/lib/config"
	"flanders/src/lib/git"
	"flanders/src/lib/invoke"
	"flanders/src/lib/journal"
	"flanders/src/lib/paths"
	"flanders/src/lib/reconcile"
	"flanders/src/lib/rules"
	"flanders/src/lib/stream"
	"flanders/src/lib/supervise"
	"flanders/src/lib/task"
	"flanders/src/lib/verify"
)

// Driver runs single Ralph iterations against a project. It is constructed once
// per run with the project's resolved config/paths/journal and is safe to reuse
// across iterations (it caches nothing about the plan between calls — that is the
// point; see package doc on store hot-reload).
type Driver struct {
	cfg     *config.Config
	paths   *paths.Paths
	journal *journal.Journal
	log     *slog.Logger

	// run executes a supervised invocation. It defaults to supervise.Run and is a
	// field ONLY so tests can substitute a stub without a real `claude` binary on
	// PATH — production code never sets it. Keeping the seam at the process boundary
	// (not, say, an interface threaded through every method) means the driver's own
	// composition logic is exercised unchanged by both real and stubbed runs.
	run func(context.Context, supervise.Spec) (*supervise.Result, error)

	// newSessionID mints the fresh per-loop session id; a field so tests can pin it
	// for deterministic journal assertions. Defaults to invoke.NewSessionID.
	newSessionID func() (string, error)

	// now is the wall clock; a field so tests get deterministic StartedAt/EndedAt.
	now func() time.Time
}

// Options carries the dependencies New needs. Config, Paths, and Journal are
// required (they are the project's resolved truth — the orchestrate startup in
// cmd/flanders already builds all three); Log is optional (nil discards).
type Options struct {
	Config  *config.Config
	Paths   *paths.Paths
	Journal *journal.Journal
	Log     *slog.Logger
}

// New builds a Driver from already-resolved project dependencies. It errors on a
// nil Config/Paths/Journal because a driver with any of those missing could never
// run a real iteration — failing here is clearer than a nil-deref mid-loop.
func New(opts Options) (*Driver, error) {
	if opts.Config == nil {
		return nil, errors.New("loop: New needs a Config")
	}
	if opts.Paths == nil {
		return nil, errors.New("loop: New needs Paths")
	}
	if opts.Journal == nil {
		return nil, errors.New("loop: New needs a Journal")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Driver{
		cfg:          opts.Config,
		paths:        opts.Paths,
		journal:      opts.Journal,
		log:          log,
		run:          supervise.Run,
		newSessionID: invoke.NewSessionID,
		now:          time.Now,
	}, nil
}

// Result is the outcome of one iteration — everything the orchestrator (Phase 5)
// needs to advance the run-state machine and the guardrails without re-deriving
// anything. When NoWork is true no agent was spawned (Observation is nil and no
// journal entry was written), so the orchestrator reads AllDone to tell "plan
// finished" from "stalled" (every remaining task blocked/active/waiting).
type Result struct {
	Phase   string     // the phase this iteration ran (build|plan|…)
	Task    *task.Task // the selected task, or nil when NoWork
	NoWork  bool       // no actionable task this iteration (nothing to spawn)
	AllDone bool       // set with NoWork: every task is `done` (the success signal)

	SessionID   string                  // the fresh session id used for the invocation
	Observation *stream.LoopObservation // folded stream-json (nil when NoWork)
	Outcome     stream.Outcome          // success | usage_limit | error (nil when NoWork)
	ExitCode    int                     // the `claude` process exit code
	TimedOut    bool                    // the iteration hit [guardrails].iteration_timeout
	JournalSeq  int                     // the journal entry written this loop (0 when NoWork)
	Duration    time.Duration           // wall-clock for the whole iteration

	// Verify is the ground-truth test-gate verdict (spec 01 §verify). It is nil
	// when the gate did not run this loop — a non-code phase (plan/split/discuss),
	// or an invocation that did not complete cleanly (a usage limit, error, or
	// timeout): in those cases there is nothing meaningful to test. When non-nil,
	// Verify.Passed() is the harness-owned "done" signal done-detection (3.7) reads.
	Verify *verify.Result

	// Reconcile is what the harness decided about the task's status after the loop
	// (spec 02 §Mutation ownership): it respected the agent's flip, promoted to
	// `done` on a passing gate, normalized a stuck `active`, or left it unchanged.
	Reconcile reconcile.Result

	// WorkHappened reports whether git showed the working tree change this loop
	// (HEAD advanced or the dirty-file set changed). It is the signal the stall
	// guardrail (task 3.9) and resume reconciliation (spec 09) read; false when the
	// target is not a git repo (no signal available).
	WorkHappened bool
	// FilesTouched is the sorted set of paths changed this loop (also recorded in
	// the journal). Empty when nothing changed or the target is not a git repo.
	FilesTouched []string

	// Checkpoint is the git commit sha created this loop (spec 01 §Checkpointing),
	// or "" when no commit was made: no progress in the default `progress` mode,
	// checkpointing off ([git].enabled=false or commit_each="off"), a clean tree, or
	// a non-repo target the run is not permitted to init. The orchestrator persists a
	// non-empty value to state.last_checkpoint (spec 09).
	Checkpoint string

	// ContextTrip is the highest context-pressure tier the loop reached (spec 01
	// §context-pressure, plan task 3.11): TripNone (stayed under soft_pct), TripSoft (a
	// graceful wind-down was steered at soft_pct), or TripHard (the harness hard-killed at
	// hard_pct and wrote `blocked: context-overreach` itself — Reconcile then reflects that
	// terminal status). The orchestrator/TUI read it to surface why a loop ended and to
	// route an over-large task to a fresh split pass.
	ContextTrip stream.Trip

	// TestVerdict is the TDD test-phase routing decision (plan task 4.4, spec 07 §test
	// agent) — set only when Iterate ran the "test" phase to a clean completion (TestVerdictNone
	// otherwise). It tells the per-task build flow (4.5) whether the acceptance is already
	// satisfied (skip build), a red acceptance test is ready (proceed to build), no red test
	// was produced (re-run the test loop), or the task was blocked. See testphase.go.
	TestVerdict TestVerdict

	// PlanComplete is the plan-completeness coverage verdict (plan task 4.3, spec 06
	// §Plan-completeness criterion) — set only by PlanIterate (nil for a task loop). It
	// is the plan-phase loop-exit signal: the orchestrator runs plan loops until
	// PlanComplete.Complete() holds, feeding PlanComplete.Uncovered into the next focused
	// re-plan. It is filled best-effort after the loop's work is recorded, so a scan
	// error (e.g. the agent wrote a malformed task file) leaves it nil without failing
	// the already-journaled loop — the caller can re-run Driver.PlanComplete itself.
	PlanComplete *Coverage
}

// Iterate runs one Ralph loop in the given phase (build|plan|test|split — the
// values config.PhaseClass understands) and returns what happened. It performs the
// full iteration anatomy: select → compose → spawn → observe → verify → evaluate →
// checkpoint, recording the loop in the journal. iter is the orchestrator's per-phase
// iteration number (state.iter), used only for the checkpoint commit message's
// {iter} variable — the driver itself owns no iteration counter (that is Phase 5's
// run-state machine).
//
// A returned error is an infrastructure failure (couldn't read the plan, build the
// command, or spawn the process) — the orchestrator should surface and halt. A
// loop that ran but produced an error *result* (an API/agent error, a usage limit,
// a timeout) is NOT an error here: it completes normally with Result.Outcome set,
// because that is a routine, recordable outcome the guardrails act on, not a
// harness fault.
//
// The "test" phase runs the same spine but as the TDD test agent (plan task 4.4, spec 07
// §test agent): compose uses the test-agent role (taskRoleHeader), a green gate does NOT
// auto-promote the task to done (it could mean "no test covers this yet"), and the
// post-loop evaluation sets Result.TestVerdict so the per-task build flow knows whether to
// skip build (already satisfied), proceed to build (a red acceptance test is ready),
// re-run the test loop (no red test produced), or drain (blocked). See testphase.go.
func (d *Driver) Iterate(ctx context.Context, phase string, iter int) (*Result, error) {
	start := d.now()

	// 1. SELECT — rebuild the store from disk (hot-reload, see package doc) and
	// pick the next actionable task. A dependency cycle is a real error (it would
	// otherwise masquerade as a finished plan); "nothing actionable" is not.
	store, err := task.LoadDir(d.paths.Tasks)
	if err != nil {
		return nil, fmt.Errorf("loop: load tasks: %w", err)
	}
	t, err := store.Next()
	if err != nil {
		return nil, fmt.Errorf("loop: select task: %w", err)
	}
	if t == nil {
		// No task to run: either the plan is complete (AllDone) or it is stalled
		// (everything left is blocked/active). The orchestrator decides what to do.
		return &Result{Phase: phase, NoWork: true, AllDone: store.AllDone()}, nil
	}

	// 2. COMPOSE — the prompt body for this task (task file + dependency outcomes +
	// referenced spec excerpts + one-line plan summary), plus the loop rules appended
	// as a system prompt (readRules: the project's .flanders/rules.md, or the built-in
	// default when that file is absent — the rules are always in force).
	prompt, err := d.composePrompt(t, store, phase)
	if err != nil {
		return nil, fmt.Errorf("loop: compose prompt for task %s: %w", t.ID(), err)
	}
	rules, err := d.readRules()
	if err != nil {
		return nil, fmt.Errorf("loop: read rules: %w", err)
	}

	// 3. SPAWN — mint a fresh session id, build the invocation argv, and supervise
	// the process. The raw transcript is teed into buf for the journal archive.
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
		return nil, fmt.Errorf("loop: build invocation: %w", err)
	}
	// Snapshot the git working tree just before the agent runs, so the after-loop
	// snapshot (step 6) reveals what THIS loop touched — the harness's own "did work
	// happen" signal (spec 02 §Mutation ownership), not the cumulative tree state.
	// Best-effort: a non-repo target yields an inert snapshot and no signal.
	pre := git.Snap(ctx, d.paths.Root)

	// Context-pressure guardrail (plan task 3.11): a per-loop guard drives a live
	// occupancy tracker off every decoded event (the supervisor's OnEvent hook) and, on
	// a trip, steers the running process — inject a graceful wind-down at soft_pct (when
	// stream_input is on) and hard-kill at hard_pct. *supervise.Proc satisfies the guard's
	// procController, so the hook hands it the live process to act on (see context_pressure.go).
	guard := newContextGuard(d.cfg, d.log, windDownMessage)
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
		return nil, fmt.Errorf("loop: spawn agent for task %s: %w", t.ID(), err)
	}

	// 4. OBSERVE — classify the outcome and write the journal entry (raw transcript
	// + summary). The journal is the only memory across fresh contexts (spec 01).
	obs := res.Observation
	if obs == nil {
		obs = &stream.LoopObservation{} // supervise guarantees non-nil; belt-and-braces
	}
	outcome := obs.Classify(res.ExitCode)

	// 5. VERIFY — the harness-owned ground-truth test gate (spec 01 §verify): run
	// the configured test command (+ optional build/lint) and trust its exit code,
	// not the agent's self-report. Gated to the code-producing phases on a clean
	// invocation (see runsTestGate); on every other loop vr stays nil and the
	// journal records Test.Ran=false ("not verified this loop").
	var vr *verify.Result
	if d.runsTestGate(phase, outcome) {
		r := verify.Run(ctx, d.paths.Root, d.cfg.Commands)
		vr = &r
		if !vr.Test.Ran {
			d.log.Debug("test gate skipped: no [commands].test configured", "phase", phase)
		} else if vr.Passed() {
			d.log.Info("test gate passed", "phase", phase, "task", t.ID(), "exit", vr.Test.ExitCode)
		} else {
			d.log.Warn("test gate failed", "phase", phase, "task", t.ID(),
				"exit", vr.Test.ExitCode, "err", vr.Test.Err, "output_tail", vr.Test.Output)
		}
	}

	// 6. EVALUATE — record the git "did work happen" signal for THIS loop, then
	// reconcile the task's status (spec 02 §Mutation ownership). Reconciliation reads
	// the task as the agent left it on disk, so re-read it here: the agent edits its
	// own `status` mid-run, and the in-memory `t` predates those edits. A read failure
	// (e.g. the agent clobbered the file) skips reconciliation rather than risk
	// rewriting an unreadable file — the journal then records the pre-loop status.
	workHappened, files := git.Diff(pre, git.Snap(ctx, d.paths.Root))

	// Context-pressure tier 3 (plan task 3.11): when the harness hard-killed this loop, it
	// OWNS recording the outcome — the agent never reached its own status flip. Write
	// `blocked: context-overreach` (plus a git-diff handoff of the partial progress above)
	// to the task file HERE, before the reconcile reload, so reconciliation sees the
	// terminal status and respects it on the normal agent-status-first path. The handoff
	// uses `files` (computed before this write), so the harness's own marker edit is not
	// counted as the loop's work; the checkpoint step then commits the marker + partial work.
	if guard.hardKilled() {
		if werr := d.markContextOverreach(t, files); werr != nil {
			d.log.Warn("context-pressure: writing block marker failed", "task", t.ID(), "err", werr)
		}
	}

	after := t
	var rec reconcile.Result
	if reloaded, rerr := task.ParseFile(t.Path); rerr == nil {
		after = reloaded
		// Promote-on-green is suppressed for the TEST phase. In the build phase a passing
		// gate proves the task done — a red acceptance test preceded it (the TDD invariant),
		// so green genuinely means the acceptance is met, and the harness records a forgotten
		// flip. In the test phase a green suite must NOT promote: it can mean "no test covers
		// this acceptance yet" (the NoRedTest case), so only the test agent's explicit,
		// gate-corroborated `done` marks satisfaction — derived in the test-phase verdict below.
		rec, err = reconcile.Reconcile(after, reconcile.Signals{
			TestRan:    vr != nil && vr.Test.Ran,
			TestPassed: vr != nil && vr.Passed() && phase != "test",
		})
		if err != nil {
			return nil, fmt.Errorf("loop: reconcile task %s: %w", t.ID(), err)
		}
	} else {
		d.log.Warn("reconcile skipped: task file unreadable after loop", "task", t.ID(), "err", rerr)
		rec = reconcile.Result{Action: reconcile.ActionUnchanged, From: t.Status(), To: t.Status()}
	}

	// TEST PHASE verdict (plan task 4.4, spec 07 §test agent). A test loop's outcome is not
	// "did the suite pass" but "is there a red acceptance test to build against, or is the
	// task already satisfied" — derive that routing decision from the reconciled status and
	// the ground-truth gate (classifyTestVerdict). The orchestrator (4.5) reads
	// Result.TestVerdict to skip or run the build loop. Computed only on a cleanly-completed
	// test loop; a non-success outcome is the orchestrator's guardrail path (Result.Outcome).
	var verdict TestVerdict
	if phase == "test" && outcome == stream.OutcomeSuccess {
		gateRan := vr != nil && vr.Test.Ran
		gatePassed := vr != nil && vr.Passed()
		verdict = classifyTestVerdict(after.Status(), gateRan, gatePassed)
		if verdict == TestVerdictRedReady && after.Status() == task.StatusDone {
			// Ground-truth correction (spec 00 decision 2): the test agent reported the
			// acceptance already satisfied (status done), but the harness gate did not pass, so
			// the claim is unconfirmed. Demote to pending so the build loop selects the task and
			// establishes green for real, rather than skipping build on an unverified done.
			after.SetStatus(task.StatusPending)
			if werr := after.WriteFile(""); werr != nil {
				return nil, fmt.Errorf("loop: correct unconfirmed test-phase done for task %s: %w", t.ID(), werr)
			}
			rec = reconcile.Result{Action: reconcile.ActionNormalized, From: rec.From, To: task.StatusPending, Wrote: true}
			d.log.Warn("test phase: agent reported satisfied but gate not green — proceeding to build",
				"task", t.ID(), "gate_exit", vr.Test.ExitCode)
		}
	}

	class, _ := d.cfg.PhaseClass(phase) // phase already validated by invoke.Build above
	sum := d.buildSummary(phase, t, after, sid, class, res, obs, start, outcome, vr, files)
	// A context-pressure hard kill (plan task 3.11) reads as a generic process error to
	// Classify (killed mid-stream, no result), so name it precisely in the journal — the
	// history must show the loop ended on the backstop, not on an opaque agent crash.
	if guard.hardKilled() {
		sum.Error = "context-pressure hard backstop (≥ context.hard_pct): harness ended the loop and marked it blocked: context-overreach"
	}
	seq, err := d.journal.Append(sum, &raw)
	if err != nil {
		return nil, fmt.Errorf("loop: journal task %s: %w", t.ID(), err)
	}

	// 7. CHECKPOINT — on progress, commit the working tree so the iteration is
	// revertable (spec 01 §Checkpointing). Best-effort: the journal entry is already
	// written above and the work is on disk, so a commit failure is logged inside
	// checkpoint, not fatal here. The new commit sha (or "") is surfaced on Result
	// for the orchestrator to persist as state.last_checkpoint (spec 09).
	checkpointSHA := d.checkpoint(ctx, phase, iter, t, after, vr)

	d.log.Info("loop iteration complete",
		"phase", phase, "task", t.ID(), "session", sid,
		"outcome", outcome.String(), "exit", res.ExitCode,
		"timed_out", res.TimedOut, "journal_seq", seq,
		"status_after", sum.StatusAfter, "reconcile", rec.Action, "work_happened", workHappened,
		"test_passed", sum.Test.Passed(), "test_verdict", string(verdict), "checkpoint", checkpointSHA,
		"context_trip", guard.peakTrip().String(), "cost_usd", obs.Cost)

	return &Result{
		Phase:        phase,
		Task:         t,
		SessionID:    sid,
		Observation:  obs,
		Outcome:      outcome,
		ExitCode:     res.ExitCode,
		TimedOut:     res.TimedOut,
		JournalSeq:   seq,
		Duration:     d.now().Sub(start),
		Verify:       vr,
		Reconcile:    rec,
		WorkHappened: workHappened,
		FilesTouched: files,
		Checkpoint:   checkpointSHA,
		ContextTrip:  guard.peakTrip(),
		TestVerdict:  verdict,
	}, nil
}

// runsTestGate reports whether the verify step (spec 01 §verify) should run this
// loop. The test gate is meaningful only for the code-producing phases — build and
// the TDD test phase — whose ground truth is the test command; plan/split/discuss
// produce specs or task files, not code, and are judged by plan-completeness (plan
// task 4.3), so running the test suite after them is wasteful and misleading. It
// also runs only on a clean invocation (OutcomeSuccess): when the loop ended in a
// usage limit, an error, or a timeout, the agent did not finish its work, so a
// verdict on the half-done tree would just confuse done-detection and the stall
// counter — the orchestrator's guardrails handle those non-success outcomes.
func (d *Driver) runsTestGate(phase string, outcome stream.Outcome) bool {
	return outcome == stream.OutcomeSuccess && (phase == "build" || phase == "test")
}

// readRules returns the loop-rules text appended to the agent's system prompt
// (spec 01 §Agent invocation). It reads the rules file (paths.Rules, default
// .flanders/rules.md) when present — that is the user-tunable copy `flanders init`
// writes. When the file is absent it falls back to the built-in rules.DefaultMarkdown
// rather than "", so the agent's loop contract is ALWAYS in force (mirroring how
// config falls back to its documented defaults): a project that never ran init, or
// one whose rules file was removed, still gets the one-task-per-loop / flip-status /
// don't-touch-harness-state / delegate-to-subagents / proactive-handoff discipline.
// Any read error other than not-exist is real and surfaced.
func (d *Driver) readRules() (string, error) {
	data, err := os.ReadFile(d.paths.Rules)
	if errors.Is(err, fs.ErrNotExist) {
		return rules.DefaultMarkdown, nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}
