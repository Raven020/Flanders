// Package orchestrate is the bare-`flanders` run loop — the outermost coordinator
// that drives a project from specs to a provably-complete build with no human in the
// loop (specs/06-orchestration.md; specs/01-ralph-loop.md §Done-detection;
// specs/09-state-and-resume.md).
//
// Where it sits. Everything below it is a tested primitive built ahead of this
// consumer: src/lib/loop drives a single Ralph iteration (Iterate), the per-task
// TDD flow (RunTask), and the plan loop (PlanIterate/PlanComplete); src/lib/usage
// is the usage-window wait/resume; src/lib/guardrail holds the stall/max-iteration
// predicates; src/lib/state is the resumable cursor. The orchestrator is the one
// piece that knows the *shape of a whole run*: the phase machine, the run-state
// transitions, and the termination condition. It owns state.json (only the harness
// writes it, spec 09) and persists at every transition so a crash resumes.
//
// The phase machine (spec 06 §Build: drain, then batch re-plan; locked):
//
//	1. PLAN  — run plan loops until plan-complete (every spec requirement maps to
//	           >=1 task, judged by the mechanical coverage scan, src/lib/loop
//	           PlanComplete). We do NOT try to prove the plan perfect up front;
//	           residual gaps are expected and surface during build as blocked tasks.
//	2. BUILD — drain: drive every actionable task through test->build->verify
//	           (RunTask). A task the plan didn't cover is marked `blocked` by the
//	           agent and the drain moves on to the next task — never a phase switch
//	           per snag.
//	3. RE-PLAN — only once the drain is exhausted (every task done or blocked), if
//	           blocked tasks remain, run ONE focused plan loop to resolve the blocks,
//	           then resume build. At most one phase switch per drain boundary — the
//	           switch is the real cost (a fresh-context spin-up), and fresh-context
//	           means nothing is lost across it.
//
// The run ends DONE when the locked three-part condition holds (spec 01/06): the
// harness's own test command exits 0, every task is `done`, and no stall is in
// effect. The agent may *report* completion, but that report is advisory and never
// the stop condition — the run-level test gate (this package, via src/lib/verify) is
// ground truth, the same philosophy that makes the per-task gate ground truth.
//
// Autonomy (spec 06 §Autonomy, locked). Once launched the pipeline is fully
// autonomous: no per-cycle approval, no plan->build gate. It pauses only on a
// guardrail HALT (stall, max-iterations, a degraded test phase, a false done) or a
// usage-window WAIT (auto-resumes at reset) — exceptional events, not routine
// approvals.
//
// Iteration-budget apportionment (spec 06 OPEN — decided here). [guardrails].
// max_iterations is applied as a PER-PHASE cap: plan loops are bounded by
// state.iter.plan and build loops by state.iter.build, each against max_iterations.
// state.iter already tracks {plan,build,total} separately, so a runaway plan phase
// cannot exhaust the budget a build phase needs. This is the conservative reading;
// a global or split cap can replace it without touching the loop primitives (they
// only compare whatever counter they are handed).
package orchestrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"flanders/src/lib/config"
	"flanders/src/lib/guardrail"
	"flanders/src/lib/loop"
	"flanders/src/lib/paths"
	"flanders/src/lib/reconcile"
	"flanders/src/lib/state"
	"flanders/src/lib/stream"
	"flanders/src/lib/task"
	"flanders/src/lib/usage"
	"flanders/src/lib/verify"
)

// driver is the loop engine the orchestrator drives. *loop.Driver satisfies it; the
// interface exists so the orchestrator's phase-machine logic can be unit-tested with
// a scripted fake (which performs the same on-disk task-file mutations the real
// driver's agent+reconcile would) without a live `claude` binary. The loop package's
// own tests already cover Iterate/RunTask/PlanIterate end-to-end against a stub
// process; this seam keeps the orchestrator's distinct concern — the run-state
// machine and phase transitions — testable on its own.
type driver interface {
	PlanIterate(ctx context.Context, iter int) (*loop.Result, error)
	PlanComplete() (*loop.Coverage, error)
	RunTask(ctx context.Context, iter, stallCount int) (*loop.TaskFlowResult, error)
}

// Summary is the terminal report (spec 06 §Termination & handoff): tasks, cost,
// iterations, duration, and how the run ended (DONE or HALTED + why).
type Summary struct {
	RunState     state.RunState // DONE on success, HALTED on a guardrail stop
	HaltReason   string         // populated when RunState==HALTED
	HaltTask     string         // the task in flight at the halt, when known
	TasksDone    int            // tasks in `done` at the end (ground truth, from disk)
	TasksBlocked int            // tasks left `blocked`
	PlanIters    int            // plan-phase loops run (state.iter.plan)
	BuildIters   int            // build-phase loops run (state.iter.build)
	TotalIters   int            // all loops (state.iter.total)
	Duration     time.Duration  // wall-clock since the run first started
	CostUSD      float64        // summed loop cost — info only, never a stop (spec 00)
}

// Options are the resolved project dependencies New needs. Driver/Config/Paths/State
// are required; StatePath is where state.json is persisted (paths.State). Log is
// optional (nil discards).
type Options struct {
	Driver    driver
	Config    *config.Config
	Paths     *paths.Paths
	State     *state.State
	StatePath string
	Log       *slog.Logger
}

// Orchestrator runs one full plan->build pipeline. It is constructed once per run
// with the project's resolved config/paths and the loaded run-state cursor.
type Orchestrator struct {
	drv       driver
	cfg       *config.Config
	paths     *paths.Paths
	st        *state.State
	statePath string
	waiter    *usage.Waiter
	log       *slog.Logger

	cost float64 // accumulated loop cost across the run (for the summary)

	// now is the wall clock; a field so the summary's duration is deterministic in
	// tests. verify runs the run-level done-gate; a field so tests substitute a
	// scripted gate without a real test command (production uses verify.Run).
	now    func() time.Time
	verify func(ctx context.Context, dir string, cmds config.Commands) verify.Result
}

// New builds an Orchestrator from resolved dependencies, erroring on any missing
// required input (a nil dependency could only fail mid-run with a less clear panic).
func New(opts Options) (*Orchestrator, error) {
	if opts.Driver == nil {
		return nil, errors.New("orchestrate: New needs a Driver")
	}
	if opts.Config == nil {
		return nil, errors.New("orchestrate: New needs a Config")
	}
	if opts.Paths == nil {
		return nil, errors.New("orchestrate: New needs Paths")
	}
	if opts.State == nil {
		return nil, errors.New("orchestrate: New needs a State")
	}
	if opts.StatePath == "" {
		return nil, errors.New("orchestrate: New needs a StatePath")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Orchestrator{
		drv:       opts.Driver,
		cfg:       opts.Config,
		paths:     opts.Paths,
		st:        opts.State,
		statePath: opts.StatePath,
		waiter:    usage.NewWaiter(opts.Config.Usage, log),
		log:       log,
		now:       func() time.Time { return time.Now().UTC() },
		verify:    func(ctx context.Context, dir string, cmds config.Commands) verify.Result { return verify.Run(ctx, dir, cmds) },
	}, nil
}

// Run drives the full pipeline to a terminal Summary (DONE or HALTED). A returned
// error is an infrastructure failure (couldn't read the plan, build a command, spawn
// a process, or the run was cancelled) — distinct from a guardrail HALT, which is a
// normal, recorded outcome carried on the Summary.
func (o *Orchestrator) Run(ctx context.Context) (*Summary, error) {
	// The run can only ever reach DONE if the harness has a test command to gate on
	// (spec 01 §done-detection; spec 03 requires [commands].test for build). Fail loudly
	// up front rather than planning+building toward an unreachable done.
	if err := o.cfg.ValidateForBuild(); err != nil {
		return nil, fmt.Errorf("orchestrate: cannot run without a buildable config: %w", err)
	}
	o.st.Stall.N = o.cfg.Guardrails.StallN

	// Resume (spec 09 §Resume / recovery). The loaded run_state tells us how a prior
	// process left off; restore terminal states, re-enter a usage wait, or reconcile a
	// crashed RUNNING loop against ground truth before continuing.
	switch o.st.RunState {
	case state.StateDone:
		o.log.Info("restored DONE state; nothing to do")
		return o.summary(), nil
	case state.StateHalted:
		// A guardrail halt is the operator's to clear (recovery UX is spec-OPEN). A bare
		// re-run does not silently retry it — that would likely re-halt immediately.
		o.log.Warn("restored HALTED state; not auto-resuming a guardrail halt — clear .flanders/state.json to retry",
			"reason", o.st.Halt.Reason, "task", o.st.Halt.Task)
		return o.summary(), nil
	case state.StateWaiting:
		// Re-enter the usage wait a prior process started: sleep the remaining time to
		// reset_at (or backoff), then RUNNING. cycles_used is not re-bumped (the window was
		// claimed when the wait began).
		if err := o.waiter.Resume(ctx, o.st, o.statePath); err != nil {
			return nil, err
		}
	case state.StateRunning:
		// Crash mid-loop: do not trust the half-finished loop. Re-derive any `active`
		// task's status from ground truth before re-selecting (spec 09).
		o.reconcileResume(ctx)
	case state.StatePaused:
		// Headless has no interactive pause; treat a restored PAUSED as resume.
		o.st.RunState = state.StateRunning
	}
	o.st.RunState = state.StateRunning
	o.persist()

	// PLAN PHASE — plan until the coverage scan says every requirement maps to a task.
	if err := o.planToComplete(ctx); err != nil {
		return nil, err
	}
	if o.stopped() {
		return o.summary(), nil
	}

	// BUILD <-> focused-replan cycle. One drain to exhaustion, then at most one focused
	// re-plan per drain boundary, repeat until done or a guardrail halts.
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		allDone, err := o.buildDrain(ctx)
		if err != nil {
			return nil, err
		}
		if o.stopped() {
			return o.summary(), nil
		}
		if allDone {
			// Run-level done-gate (spec 01/06): the harness's own test command is the
			// ground truth, not the per-task gates or the agents' reports. No stall can be
			// in effect here — a tripped stall would have halted the drain above.
			if o.doneGatePasses(ctx) {
				o.st.RunState = state.StateDone
				o.st.Halt = state.Halt{}
				o.persist()
				o.log.Info("run complete: all tasks done and the test gate is green",
					"plan_iters", o.st.Iter.Plan, "build_iters", o.st.Iter.Build)
				return o.summary(), nil
			}
			// Every task claims done but the suite is red — the false-done backstop spec 01
			// describes. Halt and surface rather than declaring a hollow success.
			o.halt("all tasks report done but the harness test command did not pass (false done)", "")
			return o.summary(), nil
		}

		// Drain exhausted with work remaining ⇒ blocked tasks. One focused plan loop to
		// resolve the blocks, then resume build (spec 06).
		progressed, err := o.focusedReplan(ctx)
		if err != nil {
			return nil, err
		}
		if o.stopped() {
			return o.summary(), nil
		}
		if !progressed {
			// The re-plan created no new actionable work — looping again would drain
			// straight back here. Stop rather than spin (bounds the re-plan cycle).
			o.halt("re-plan produced no new actionable work; remaining tasks stay blocked", "")
			return o.summary(), nil
		}
	}
}

// planToComplete runs plan loops until the plan-completeness scan passes, the
// max-iterations cap trips, or the plan stalls. On a project whose plan is already
// complete (a resume) it returns immediately without spawning an agent.
func (o *Orchestrator) planToComplete(ctx context.Context) error {
	o.setPhase(state.PhasePlan)
	o.persist()

	if cov, err := o.drv.PlanComplete(); err == nil && cov != nil && cov.Complete() {
		o.log.Info("plan already complete; skipping plan phase", "requirements", cov.Requirements)
		return nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if guardrail.MaxIterationsReached(o.st.Iter.Plan, o.cfg.Guardrails.MaxIterations) {
			o.halt("max iterations reached in plan phase", "")
			return nil
		}
		res, err := o.drv.PlanIterate(ctx, o.st.Iter.Plan)
		if err != nil {
			return err
		}
		o.st.Iter.Plan++
		o.st.Iter.Total++
		o.recordLoop(res)
		o.st.Stall.Count = guardrail.StallStep(o.st.Stall.Count, res.WorkHappened)
		o.persist()

		if stop, werr := o.handleAgentOutcome(ctx, res); werr != nil {
			return werr
		} else if stop {
			return nil // usage-halt: st is HALTED, surfaced by the caller
		}

		if res.PlanComplete != nil && res.PlanComplete.Complete() {
			o.log.Info("plan complete", "plan_iters", o.st.Iter.Plan, "requirements", res.PlanComplete.Requirements)
			return nil
		}
		if guardrail.StallTripped(o.st.Stall.Count, o.cfg.Guardrails.StallN) {
			o.halt("plan phase stalled (consecutive loops produced no task-file changes)", "")
			return nil
		}
	}
}

// buildDrain drives actionable tasks through RunTask until the drain is exhausted
// (DispNoWork) or a guardrail halts. It returns allDone from the terminating
// DispNoWork; a halt is reflected on o.st (the caller checks o.stopped()).
func (o *Orchestrator) buildDrain(ctx context.Context) (allDone bool, err error) {
	o.setPhase(state.PhaseBuild)
	o.persist()

	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		// iter is the cumulative build-phase loop count, so RunTask honors the per-phase
		// max_iterations cap across its inner test+build loops; stallCount is threaded so a
		// no-progress task cannot burn the whole phase before the stall guardrail fires.
		tfr, rerr := o.drv.RunTask(ctx, o.st.Iter.Build, o.st.Stall.Count)
		if rerr != nil {
			return false, rerr
		}
		o.recordTaskFlow(tfr)
		o.persist()

		switch tfr.Disposition {
		case loop.DispNoWork:
			// The drain is exhausted: every task is done (success candidate) or blocked.
			return tfr.AllDone, nil
		case loop.DispDone, loop.DispBlocked:
			// Task reached a terminal status; move on to the next actionable task.
			continue
		case loop.DispStalled:
			o.halt("stall: consecutive loops made no progress (no file change, no status change)", taskID(tfr))
			return false, nil
		case loop.DispMaxIterations:
			o.halt("max iterations reached in build phase", taskID(tfr))
			return false, nil
		case loop.DispNoRedTest:
			// The test agent never produced a red acceptance test across its bounded retries
			// — a degraded case spec 07 does not enumerate. Surface it rather than building
			// against no test (the TDD invariant) or spinning. (Deferring it to a re-plan is a
			// possible future refinement.)
			o.halt("test phase produced no red acceptance test for the task", taskID(tfr))
			return false, nil
		case loop.DispUsageLimit:
			stop, werr := o.handleUsage(ctx, resetOf(tfr.Last))
			if werr != nil {
				return false, werr
			}
			if stop {
				return false, nil // usage on_limit=halt or max_cycles reached
			}
			continue // window reset; re-enter the drain
		default: // DispTimeout, DispError — a non-clean loop. Let the stall guardrail catch a
			// persistent failure (RunTask returns these before its own stall check), else retry.
			if guardrail.StallTripped(o.st.Stall.Count, o.cfg.Guardrails.StallN) {
				o.halt("repeated loop errors/timeouts with no progress", taskID(tfr))
				return false, nil
			}
			continue
		}
	}
}

// focusedReplan runs exactly ONE plan loop to resolve the blocks a drain surfaced,
// then returns to the build phase (spec 06: "one focused plan loop … then resumes
// build"). progressed reports whether the re-plan left an actionable task — if not,
// the caller stops rather than looping into an immediately-re-exhausted drain.
func (o *Orchestrator) focusedReplan(ctx context.Context) (progressed bool, err error) {
	o.setPhase(state.PhasePlan)
	o.persist()

	if guardrail.MaxIterationsReached(o.st.Iter.Plan, o.cfg.Guardrails.MaxIterations) {
		o.halt("max iterations reached during re-plan", "")
		return false, nil
	}
	res, err := o.drv.PlanIterate(ctx, o.st.Iter.Plan)
	if err != nil {
		return false, err
	}
	o.st.Iter.Plan++
	o.st.Iter.Total++
	o.recordLoop(res)
	o.st.Stall.Count = guardrail.StallStep(o.st.Stall.Count, res.WorkHappened)
	o.persist()

	if stop, werr := o.handleAgentOutcome(ctx, res); werr != nil {
		return false, werr
	} else if stop {
		return false, nil
	}

	o.setPhase(state.PhaseBuild)
	o.persist()

	store, err := o.loadStore()
	if err != nil {
		return false, err
	}
	next, nerr := store.Next()
	if nerr != nil {
		// A dependency cycle the re-plan introduced is a real error to surface.
		return false, fmt.Errorf("orchestrate: re-select after re-plan: %w", nerr)
	}
	return next != nil, nil
}

// handleAgentOutcome reacts to a plan loop's invocation outcome. A usage limit waits
// (or halts per [usage].on_limit / max_cycles); a genuine error or timeout is a
// no-progress loop the stall counter already absorbed, so the run continues (a
// persistent failure trips stall). It returns stop=true only when the run should
// end (a usage halt — st is left HALTED).
func (o *Orchestrator) handleAgentOutcome(ctx context.Context, res *loop.Result) (stop bool, err error) {
	if res == nil || res.Outcome == stream.OutcomeUsageLimit && res.Observation == nil {
		return false, nil
	}
	if res.Outcome == stream.OutcomeUsageLimit {
		return o.handleUsage(ctx, resetOf(res))
	}
	return false, nil
}

// handleUsage parks the run in a usage-window wait and resumes at reset (or halts per
// [usage].on_limit / max_cycles). The Waiter persists every transition itself, so a
// crash mid-wait resumes via the WAITING state on disk. stop=true means the run
// halted (st is HALTED); a returned error is a cancelled wait (st stays WAITING) or a
// persist failure.
func (o *Orchestrator) handleUsage(ctx context.Context, resetAt *time.Time) (stop bool, err error) {
	dec, werr := o.waiter.HandleLimit(ctx, o.st, o.statePath, resetAt)
	if werr != nil {
		return true, werr
	}
	if dec.Action == usage.ActionHalt {
		return true, nil // st HALTED by the waiter
	}
	return false, nil // waited and resumed to RUNNING
}

// reconcileResume re-derives ground truth after a crash mid-loop (spec 09). It does
// not trust the half-finished loop: any task left `active` is normalized back to
// `pending` via the same reconcile policy a loop uses, so the next selection re-runs
// it with fresh context. (Git-diff enrichment — crediting work a crashed loop landed
// but never flipped status for — is a documented future refinement; normalizing to
// pending is the safe, correct floor.)
func (o *Orchestrator) reconcileResume(ctx context.Context) {
	store, err := o.loadStore()
	if err != nil {
		o.log.Warn("resume reconcile: load tasks failed", "err", err)
		return
	}
	for _, t := range store.Tasks() {
		if t.Status() != task.StatusActive {
			continue
		}
		rec, rerr := reconcile.Reconcile(t, reconcile.Signals{})
		if rerr != nil {
			o.log.Warn("resume reconcile failed", "task", t.ID(), "err", rerr)
			continue
		}
		o.log.Info("resume reconcile: normalized interrupted task", "task", t.ID(), "from", rec.From, "to", rec.To)
	}
}

// doneGatePasses runs the harness-owned run-level test gate (spec 01 §done-detection)
// — the ground truth for "done", independent of any agent report.
func (o *Orchestrator) doneGatePasses(ctx context.Context) bool {
	r := o.verify(ctx, o.paths.Root, o.cfg.Commands)
	if !r.Passed() {
		o.log.Warn("run-level test gate not green", "ran", r.Test.Ran, "exit", r.Test.ExitCode)
	}
	return r.Passed()
}

// recordLoop folds one agent loop's bookkeeping into state and the running cost: the
// session id and any new checkpoint sha for resume cross-reference, plus the loop's
// cost (info only, never a stop — spec 00).
func (o *Orchestrator) recordLoop(res *loop.Result) {
	if res == nil {
		return
	}
	if res.SessionID != "" {
		o.st.LastSessionID = res.SessionID
	}
	if res.Checkpoint != "" {
		o.st.LastCheckpoint = res.Checkpoint
	}
	if res.Observation != nil {
		o.cost += res.Observation.Cost
	}
}

// recordTaskFlow folds a per-task flow's bookkeeping into state: the build-phase
// iteration count (RunTask may run several inner loops), the threaded stall counter,
// the current task, and each inner loop's session/checkpoint/cost.
func (o *Orchestrator) recordTaskFlow(tfr *loop.TaskFlowResult) {
	if tfr == nil {
		return
	}
	if tfr.Task != nil {
		o.st.CurrentTask = tfr.Task.ID()
	}
	for _, r := range tfr.Results {
		o.recordLoop(r)
	}
	o.st.Iter.Build += tfr.Iterations
	o.st.Iter.Total += tfr.Iterations
	o.st.Stall.Count = tfr.StallCount
}

// setPhase records a phase transition (logged once on change).
func (o *Orchestrator) setPhase(p state.Phase) {
	if o.st.Phase != p {
		o.log.Info("phase change", "from", o.st.Phase, "to", p)
		o.st.Phase = p
	}
}

// halt records a guardrail stop: HALTED + the reason/task for the recovery UX, then
// persists so a restart restores it (spec 09).
func (o *Orchestrator) halt(reason, taskID string) {
	o.st.RunState = state.StateHalted
	o.st.Halt = state.Halt{Reason: reason, Task: taskID}
	o.persist()
	o.log.Warn("run halted", "reason", reason, "task", taskID)
}

// stopped reports whether the run reached a terminal run-state and should not
// continue the phase machine (HALTED or DONE).
func (o *Orchestrator) stopped() bool {
	return o.st.RunState == state.StateHalted || o.st.RunState == state.StateDone
}

// persist writes the state cache. A failure is logged, not fatal: state.json is a
// cache (spec 09), so a failed write only costs the next resume — halting the whole
// run over it would be worse.
func (o *Orchestrator) persist() {
	if err := o.st.Save(o.statePath); err != nil {
		o.log.Error("state persist failed", "path", o.statePath, "err", err)
	}
}

// loadStore reads the task files from disk (ground truth) for the phase-boundary
// decisions the orchestrator owns: whether the build drained to all-done, and
// whether a re-plan left actionable work.
func (o *Orchestrator) loadStore() (*task.Store, error) {
	return task.LoadDir(o.paths.Tasks)
}

// summary builds the terminal report. Task counts come from disk (ground truth);
// iteration/cost/duration come from the run-state the orchestrator maintained.
func (o *Orchestrator) summary() *Summary {
	s := &Summary{
		RunState:   o.st.RunState,
		HaltReason: o.st.Halt.Reason,
		HaltTask:   o.st.Halt.Task,
		PlanIters:  o.st.Iter.Plan,
		BuildIters: o.st.Iter.Build,
		TotalIters: o.st.Iter.Total,
		Duration:   o.now().Sub(o.st.StartedAt),
		CostUSD:    o.cost,
	}
	if store, err := o.loadStore(); err == nil {
		for _, t := range store.Tasks() {
			switch t.Status() {
			case task.StatusDone:
				s.TasksDone++
			case task.StatusBlocked:
				s.TasksBlocked++
			}
		}
	}
	return s
}

// taskID is the in-flight task id for a flow, or "" when none (DispNoWork).
func taskID(tfr *loop.TaskFlowResult) string {
	if tfr != nil && tfr.Task != nil {
		return tfr.Task.ID()
	}
	return ""
}

// resetOf is the parsed usage-window reset from a loop's observation (nil → fall back
// to [usage].backoff in the waiter).
func resetOf(res *loop.Result) *time.Time {
	if res != nil && res.Observation != nil {
		return res.Observation.ResetAt
	}
	return nil
}
