package stream

// Default context-pressure trip fractions, mirroring specs/03 [context] defaults.
// A Tracker built with a non-positive soft/hard pct falls back to these so the
// zero-ish case is safe in tests and before config is loaded; production passes
// the configured [context].soft_pct / hard_pct.
const (
	DefaultSoftPct = 0.75
	DefaultHardPct = 0.90
)

// Trip is the context-pressure tier a [Tracker] has reached — the three-tier
// guardrail of specs/01 §context-pressure, mapped to the action the loop driver
// (Phase 3) takes:
//
//   - TripSoft (~75%): inject a graceful "wrap up — mark blocked: context-overreach,
//     write a handoff, commit, end" message into the running session.
//   - TripHard (~90%): hard-kill the process; the harness writes
//     blocked: context-overreach to the task file itself, so the signal is never lost.
type Trip int

const (
	TripNone Trip = iota // below soft_pct
	TripSoft             // ≥ soft_pct (~75%): graceful wind-down
	TripHard             // ≥ hard_pct (~90%): hard-kill backstop
)

func (t Trip) String() string {
	switch t {
	case TripSoft:
		return "soft"
	case TripHard:
		return "hard"
	default:
		return "none"
	}
}

// Tracker maintains a LIVE, monotonic context-occupancy estimate for the lead
// agent over one loop. It is fed each decoded event as it arrives — the seam is
// [ObserveFunc]'s per-event hook (or a [Stream] channel) — and folds lead-agent
// token usage into a running high-water mark. From that it derives the occupancy
// fraction of the model's window and the soft/hard context-pressure trips that
// drive the guardrail (specs/01) and the TUI meter (specs/04 §meters).
//
// Why a separate type from [LoopObservation]: the observation is the post-hoc
// summary of a finished loop, whereas the Tracker is the in-flight signal the
// guardrail must react to mid-loop, before any result event exists. Both fold the
// SAME lead-only usage (via leadUsage), so they can never disagree about
// occupancy — one source of truth for "what counts as lead context."
//
// Lead-only: subagent usage is excluded (a subagent runs in its own window and
// must not inflate the lead's pressure — specs/08 §attribution).
//
// Monotonic by construction: peak is a high-water mark and the window only ever
// resolves upward (0 → reported), so Occupancy and Trip never regress — once a
// tier trips it stays tripped, which is the safe one-way guardrail decision for an
// unattended run.
type Tracker struct {
	window  int
	softPct float64
	hardPct float64
	peak    int // lead-agent token high-water mark
}

// NewTracker builds a tracker for the given context window (config's
// [context].window_tokens; 0 means "unknown — adopt the window the CLI reports at
// result time"). soft/hard are the trip fractions; non-positive values fall back
// to the spec defaults.
func NewTracker(window int, soft, hard float64) *Tracker {
	if soft <= 0 {
		soft = DefaultSoftPct
	}
	if hard <= 0 {
		hard = DefaultHardPct
	}
	return &Tracker{window: window, softPct: soft, hardPct: hard}
}

// Update folds one event into the running estimate. Lead-agent usage advances the
// token high-water mark; a result event whose modelUsage reports a context window
// adopts that window when none was configured — so occupancy becomes available
// once the first loop completes even with window_tokens = 0 (specs/08 §context
// window: contextWindow is reported only at result time, so it cannot seed the
// live % during the first turn).
func (t *Tracker) Update(ev *Event) {
	if tok, ok := leadUsage(ev); ok && tok > t.peak {
		t.peak = tok
	}
	if t.window <= 0 && ev.Type == TypeResult && ev.Result != nil {
		for _, mu := range ev.Result.ModelUsage {
			if mu.ContextWindow > t.window {
				t.window = mu.ContextWindow
			}
		}
	}
}

// Tokens is the lead-agent context-occupancy high-water mark seen so far.
func (t *Tracker) Tokens() int { return t.peak }

// Window is the context window in use (0 if still unknown).
func (t *Tracker) Window() int { return t.window }

// Occupancy is the fraction of the window currently occupied, or 0 if the window
// is still unknown. It can exceed 1 if the window estimate is low.
func (t *Tracker) Occupancy() float64 {
	if t.window <= 0 {
		return 0
	}
	return float64(t.peak) / float64(t.window)
}

// Trip is the context-pressure tier for the current occupancy. With no known
// window it is always TripNone (the meter shows nothing and the guardrail stays
// quiet until usage can be expressed as a %).
func (t *Tracker) Trip() Trip {
	if t.window <= 0 {
		return TripNone
	}
	occ := t.Occupancy()
	switch {
	case occ >= t.hardPct:
		return TripHard
	case occ >= t.softPct:
		return TripSoft
	default:
		return TripNone
	}
}

// SoftTripped reports whether occupancy has reached the soft threshold (this is
// true throughout the hard band too — the soft action subsumes the hard one).
func (t *Tracker) SoftTripped() bool { return t.Trip() >= TripSoft }

// HardTripped reports whether occupancy has reached the hard threshold.
func (t *Tracker) HardTripped() bool { return t.Trip() == TripHard }
