// Package loop is the Ralph iteration driver — the engine that runs ONE loop of
// the iteration anatomy (specs/01-ralph-loop.md §Iteration anatomy):
//
//  1. select  — hot-reload the task store from disk, pick the next actionable task
//  2. compose — build the prompt for that task (+ loop rules via system prompt)
//  3. spawn   — invoke a fresh `claude -p` session and supervise it
//  4. observe — fold the stream-json into a LoopObservation, classify the outcome
//     (+ archive the raw transcript and a summary to the journal)
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
// Why status mutation is NOT done here. Spec 02 §Mutation ownership locks that the
// agent flips its task's `status` (`active` while working, `done`/`blocked` on
// exit) and the harness cross-checks via git diff + the test command. The driver
// therefore reads the task's status before and after the loop (so the journal
// records the transition the agent made) but does not itself write status — the
// verify/reconcile step (plan task 3.5) owns the inference fallback and the
// harness-writes-status-on-kill path.
//
// What this package deliberately defers (each is its own plan item, wired into the
// same Iterate spine when it lands): the test gate / verify step (3.4), status
// reconciliation (3.5), git checkpointing (3.6), and the context/stall/usage
// guardrails (3.8–3.12). The prompt composition here is the minimal version (the
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
	"flanders/src/lib/invoke"
	"flanders/src/lib/journal"
	"flanders/src/lib/paths"
	"flanders/src/lib/stream"
	"flanders/src/lib/supervise"
	"flanders/src/lib/task"
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

	class, _ := d.cfg.PhaseClass(phase) // phase already validated by invoke.Build above
	sum := d.buildSummary(phase, t, sid, class, res, obs, start, outcome)
	seq, err := d.journal.Append(sum, &raw)
	if err != nil {
		return nil, fmt.Errorf("loop: journal task %s: %w", t.ID(), err)
	}

	d.log.Info("loop iteration complete",
		"phase", phase, "task", t.ID(), "session", sid,
		"outcome", outcome.String(), "exit", res.ExitCode,
		"timed_out", res.TimedOut, "journal_seq", seq,
		"status_after", sum.StatusAfter, "cost_usd", obs.Cost)

	return &Result{
		Phase:       phase,
		Task:        t,
		SessionID:   sid,
		Observation: obs,
		Outcome:     outcome,
		ExitCode:    res.ExitCode,
		TimedOut:    res.TimedOut,
		JournalSeq:  seq,
		Duration:    d.now().Sub(start),
	}, nil
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
