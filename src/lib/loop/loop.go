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
// done-detection (plan task 3.7). What this package still defers (each its own
// plan item, wired into the same Iterate spine when it lands): git checkpointing
// (3.6) and the context/stall/usage guardrails (3.8–3.12). The prompt composition
// here is the minimal version (the
// task file + a one-line plan summary); the richer composition (dependency
// outcomes, named spec excerpts) is plan task 3.2. None of these are stubbed —
// they are simply not yet steps of the iteration; the driver returns the
// observation and selected task so the orchestrator has what the later steps need.
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
}

// Iterate runs one Ralph loop in the given phase (build|plan|test|split — the
// values config.PhaseClass understands) and returns what happened. It performs
// steps 1–4 of the iteration anatomy (select → compose → spawn → observe) and
// records the loop in the journal; the verify/evaluate/checkpoint steps are added
// by later plan tasks (see package doc).
//
// A returned error is an infrastructure failure (couldn't read the plan, build the
// command, or spawn the process) — the orchestrator should surface and halt. A
// loop that ran but produced an error *result* (an API/agent error, a usage limit,
// a timeout) is NOT an error here: it completes normally with Result.Outcome set,
// because that is a routine, recordable outcome the guardrails act on, not a
// harness fault.
func (d *Driver) Iterate(ctx context.Context, phase string) (*Result, error) {
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

	// 2. COMPOSE — the prompt body for this task, plus the loop rules appended as a
	// system prompt. Rules are optional until plan task 3.3 authors them.
	prompt, err := composePrompt(t, store)
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

	var raw bytes.Buffer
	res, err := d.run(ctx, supervise.Spec{
		Command:     cmd,
		Prompt:      prompt,
		StreamInput: d.cfg.Agent.StreamInput,
		Timeout:     d.cfg.Guardrails.IterationTimeout.Duration,
		RawSink:     &raw,
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

	after := t
	var rec reconcile.Result
	if reloaded, rerr := task.ParseFile(t.Path); rerr == nil {
		after = reloaded
		rec, err = reconcile.Reconcile(after, reconcile.Signals{
			TestRan:    vr != nil && vr.Test.Ran,
			TestPassed: vr != nil && vr.Passed(),
		})
		if err != nil {
			return nil, fmt.Errorf("loop: reconcile task %s: %w", t.ID(), err)
		}
	} else {
		d.log.Warn("reconcile skipped: task file unreadable after loop", "task", t.ID(), "err", rerr)
		rec = reconcile.Result{Action: reconcile.ActionUnchanged, From: t.Status(), To: t.Status()}
	}

	class, _ := d.cfg.PhaseClass(phase) // phase already validated by invoke.Build above
	sum := d.buildSummary(phase, t, after, sid, class, res, obs, start, outcome, vr, files)
	seq, err := d.journal.Append(sum, &raw)
	if err != nil {
		return nil, fmt.Errorf("loop: journal task %s: %w", t.ID(), err)
	}

	d.log.Info("loop iteration complete",
		"phase", phase, "task", t.ID(), "session", sid,
		"outcome", outcome.String(), "exit", res.ExitCode,
		"timed_out", res.TimedOut, "journal_seq", seq,
		"status_after", sum.StatusAfter, "reconcile", rec.Action, "work_happened", workHappened,
		"test_passed", sum.Test.Passed(), "cost_usd", obs.Cost)

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
// (spec 01 §Agent invocation). The rules file (paths.Rules, default
// .flanders/rules.md) is authored by plan task 3.3; until it exists an absent file
// is normal and yields "" (invoke.Build then omits --append-system-prompt). Any
// other read error is real and surfaced.
func (d *Driver) readRules() (string, error) {
	data, err := os.ReadFile(d.paths.Rules)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}
