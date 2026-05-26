package loop

import (
	"log/slog"
	"strings"
	"sync"

	"flanders/src/lib/config"
	"flanders/src/lib/stream"
	"flanders/src/lib/task"
)

// The context-pressure guardrail (plan task 3.11) is the harness's defense against a
// single fresh-context loop running too deep. Past ~80% of the model's window quality
// degrades and auto-compaction risks mangling the task (spec 01 §context-pressure), so
// crossing the threshold is treated as a GRANULARITY signal — the task was too big for
// one pass — and the loop is ended with the task marked `blocked: context-overreach`.
// The split itself is always done FRESH by a clean-context agent (the split pass, plan
// task 4.6); the exhausted loop never splits itself, because "an agent out of context is
// the worst at decomposition" (spec 06 §Refinement).
//
// The block marker is guaranteed three ways, in order of preference (spec 01):
//
//  1. Proactive (agent judgment). A loop rule (rules.DefaultMarkdown, plan task 3.3)
//     tells the agent to stop early and self-block when it realizes the task is bigger
//     than one pass. This needs no code here — it is in the system prompt.
//  2. Soft wind-down (~soft_pct, harness-steered). When the live tracker crosses
//     [context].soft_pct, the harness injects a "wrap up now" message into the running
//     session over stdin (--input-format stream-json) so the agent winds down gracefully
//     and leaves its own handoff note. Available ONLY when [agent].stream_input is on.
//  3. Hard backstop (~hard_pct, or non-compliance). When the tracker crosses
//     [context].hard_pct, the harness kills the process and — because it knows why it
//     killed — writes `blocked: context-overreach` to the task file itself, plus a
//     git-diff summary of partial progress, so the signal is never lost.
//
// Fallthrough when stream_input=false: tier 2 is the only thing that needs the stdin
// channel, so with stream_input off the guard skips it and goes straight from tier 1
// (the rule) to tier 3 (hard kill at hard_pct) — simpler, but no graceful handoff note
// (spec 03 §stream_input).

// windDownMessage is the tier-2 soft wind-down the harness injects at soft_pct (spec 01
// §context-pressure: "wrap up now: mark blocked context-overreach, write handoff, commit,
// end"). It steers the agent to leave the SAME handoff the tier-3 backstop would, but
// gracefully and in the agent's own words — and explicitly forbids self-splitting, since
// decomposition must be done fresh (spec 06 §Refinement).
const windDownMessage = "⚠ CONTEXT-PRESSURE WIND-DOWN (from the Flanders harness). You are past " +
	"~75% of the context window, where quality degrades and auto-compaction risks mangling the " +
	"task. Stop starting new work now and wrap up THIS loop gracefully: (1) set this task's " +
	"`status: blocked` and `reason: context-overreach`; (2) write a handoff note in the task's " +
	"`notes` field — what you finished, what remains, and 2–4 suggested smaller sub-tasks; " +
	"(3) commit your work; (4) end the session. Do NOT split the task into new files yourself — " +
	"a fresh clean-context agent will do that from your handoff."

// procController is the subset of *supervise.Proc the context-pressure guardrail drives:
// inject the soft wind-down (tier 2) and hard-kill (tier 3). *supervise.Proc satisfies it
// (its Inject/Kill are safe to call from the OnEvent hook); defining it as an interface
// lets the guard logic be unit-tested with a fake controller, since a real *supervise.Proc
// cannot be constructed outside its package.
type procController interface {
	Inject(text string) error
	Kill()
}

// contextGuard carries the per-loop context-pressure state. It is driven event-by-event
// from the supervisor's OnEvent hook (the read goroutine), which feeds the live
// [stream.Tracker] and, on a trip, takes the tier action against the running process.
// Its decision flags are read by the driver AFTER the run completes (the supervisor's
// Wait joins the read goroutine, establishing happens-before); the mutex guards the
// flags so the live writes and the post-loop reads are race-free regardless.
type contextGuard struct {
	tracker     *stream.Tracker
	streamInput bool
	log         *slog.Logger

	mu       sync.Mutex
	peak     stream.Trip // highest tier reached this loop (for the journal / orchestrator)
	injected bool        // tier-2 wind-down already sent (inject exactly once)
	killed   bool        // tier-3 backstop fired (the harness owns the block marker)
}

// newContextGuard builds the guardrail for one loop from the project config. The tracker
// is seeded with [context].window_tokens (0 ⇒ adopt the window the CLI reports at result
// time) and the configured soft/hard fractions.
func newContextGuard(cfg *config.Config, log *slog.Logger) *contextGuard {
	return &contextGuard{
		tracker:     stream.NewTracker(cfg.Context.WindowTokens, cfg.Context.SoftPct, cfg.Context.HardPct),
		streamInput: cfg.Agent.StreamInput,
		log:         log,
	}
}

// handle folds one decoded event into the live occupancy estimate and, when it crosses a
// threshold, takes the tier action against the running process. It is invoked serially
// from the supervisor's single read goroutine (never concurrently with itself), and must
// not block — Inject writes one small line to the stdin pipe and Kill signals the group,
// both non-blocking in practice (supervise.Spec.OnEvent contract).
//
// p is the live process being supervised, taken as a [procController] so the guard logic
// is testable with a fake; in production the driver passes the supervisor's *Proc.
func (g *contextGuard) handle(p procController, ev *stream.Event) {
	g.tracker.Update(ev)
	trip := g.tracker.Trip()

	g.mu.Lock()
	if trip > g.peak {
		g.peak = trip
	}
	g.mu.Unlock()

	switch trip {
	case stream.TripHard:
		// Tier 3 (≥ hard_pct, or non-compliance after a soft wind-down): kill the process
		// group exactly once. Buffered events read after the kill re-enter here, but the
		// `killed` guard prevents a second kill. The harness now OWNS the block marker
		// (written post-loop by the driver), since the agent never got to flip its status.
		g.mu.Lock()
		first := !g.killed
		g.killed = true
		g.mu.Unlock()
		if first {
			g.log.Warn("context-pressure: hard backstop tripped — killing loop",
				"occupancy", g.tracker.Occupancy(), "tokens", g.tracker.Tokens(), "window", g.tracker.Window())
			p.Kill()
		}

	case stream.TripSoft:
		// Tier 2 (≥ soft_pct): inject a graceful "wrap up" message exactly once — but only
		// over the stdin channel, which exists only when stream_input is on. With it off
		// there is no soft channel, so we do nothing and let the tier-3 hard kill at
		// hard_pct handle it (the documented fallthrough, spec 03 §stream_input).
		if !g.streamInput {
			return
		}
		g.mu.Lock()
		first := !g.injected
		g.injected = true
		g.mu.Unlock()
		if first {
			g.log.Info("context-pressure: soft wind-down tripped — injecting wrap-up",
				"occupancy", g.tracker.Occupancy(), "tokens", g.tracker.Tokens())
			if err := p.Inject(windDownMessage); err != nil {
				// Best-effort: a failed inject (e.g. stdin already closed as the agent
				// exits) just means the hard backstop will catch it if pressure keeps rising.
				g.log.Warn("context-pressure: injecting wind-down failed", "err", err)
			}
		}
	}
}

// hardKilled reports whether the tier-3 backstop fired this loop — i.e. the harness ended
// the loop and therefore OWNS writing the block marker.
func (g *contextGuard) hardKilled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.killed
}

// peakTrip is the highest context-pressure tier reached this loop (None/Soft/Hard),
// surfaced on the loop Result for the orchestrator and the TUI meter.
func (g *contextGuard) peakTrip() stream.Trip {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak
}

// markContextOverreach is the tier-3 harness-owned write (spec 01 §context-pressure): it
// sets the killed loop's task to `blocked: context-overreach` and records a handoff note
// summarizing the partial progress (the files the loop touched). It re-reads the task off
// disk first so the marker layers onto whatever partial edits the agent made before the
// kill, falling back to the pre-loop copy if the file is unreadable — a marker on the
// original beats losing the signal. The driver calls this BEFORE the reconcile reload, so
// reconciliation sees the terminal status and respects it via the normal path.
func (d *Driver) markContextOverreach(t *task.Task, files []string) error {
	cur, err := task.ParseFile(t.Path)
	if err != nil {
		d.log.Warn("context-pressure: re-reading task before marking failed; using pre-loop copy",
			"task", t.ID(), "err", err)
		cur = t
	}
	cur.SetBlocked(task.ReasonContextOverreach)
	cur.SetNotes(contextOverreachHandoff(files))
	return cur.WriteFile(t.Path)
}

// contextOverreachHandoff renders the partial-progress handoff the tier-3 backstop writes
// into the task's `notes` (spec 01: "plus a git-diff summary of partial progress"). It is
// the input a fresh split pass (plan task 4.6) reads to decompose the over-large task — so
// it names what changed and states plainly that the split must be done fresh, never by the
// exhausted loop (spec 06 §Refinement).
func contextOverreachHandoff(files []string) string {
	var b strings.Builder
	b.WriteString("Harness-written handoff (context-pressure hard backstop, spec 01 §context-pressure). ")
	b.WriteString("This loop crossed the hard context-occupancy threshold and was ended by the harness ")
	b.WriteString("before the agent could flip its own status, so the harness recorded ")
	b.WriteString("`blocked: context-overreach` to preserve the signal. ")
	if len(files) > 0 {
		b.WriteString("Partial progress touched: ")
		b.WriteString(strings.Join(files, ", "))
		b.WriteString(". ")
	} else {
		b.WriteString("No working-tree changes were detected this loop. ")
	}
	b.WriteString("This task was too big for one fresh-context pass — a fresh split pass should decompose ")
	b.WriteString("it into 2–4 smaller tasks (the exhausted loop must not split itself, spec 06 §Refinement).")
	return b.String()
}
