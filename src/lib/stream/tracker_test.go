package stream

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// deltaLine builds a synthetic stream_event/message_delta line whose cumulative
// usage totals `total` tokens (all in input_tokens; Total() sums every category,
// so the split doesn't matter for occupancy). This is the live mid-turn signal
// the context-pressure guardrail reads.
func deltaLine(total int) string {
	return fmt.Sprintf(`{"type":"stream_event","event":{"type":"message_delta","usage":{"input_tokens":%d}}}`, total)
}

// startLine builds a message_start line carrying usage under event.message.usage
// (the fallback source when no message_delta is present).
func startLine(total int) string {
	return fmt.Sprintf(`{"type":"stream_event","event":{"type":"message_start","message":{"usage":{"input_tokens":%d}}}}`, total)
}

// subLine is a message_delta from a SUBAGENT (non-empty parent_tool_use_id): its
// usage must be ignored — a subagent runs in its own window.
func subLine(total int) string {
	return fmt.Sprintf(`{"type":"stream_event","parent_tool_use_id":"sub1","event":{"type":"message_delta","usage":{"input_tokens":%d}}}`, total)
}

// feed decodes a synthetic NDJSON stream and folds each event into tr, calling
// after() with the running occupancy after every event so tests can assert
// behavior across the stream (not just at the end).
func feed(t *testing.T, tr *Tracker, lines []string, after func(occ float64)) {
	t.Helper()
	d := NewDecoder(strings.NewReader(strings.Join(lines, "\n")), nil)
	for {
		ev, err := d.Next()
		if err != nil {
			return
		}
		tr.Update(ev)
		if after != nil {
			after(tr.Occupancy())
		}
	}
}

// TestTrackerMonotonic is the spec-2.2 acceptance: a synthetic stream drives the
// occupancy % monotonically. A transient dip in reported usage must NOT lower the
// estimate — the high-water mark only rises.
func TestTrackerMonotonic(t *testing.T) {
	tr := NewTracker(200000, 0.75, 0.90)
	lines := []string{
		startLine(1000),
		deltaLine(50000),
		deltaLine(30000), // a dip — must not move the peak back
		deltaLine(160000),
	}
	var prev float64
	feed(t, tr, lines, func(occ float64) {
		if occ < prev {
			t.Errorf("occupancy went backwards: %v < %v", occ, prev)
		}
		prev = occ
	})
	if tr.Tokens() != 160000 {
		t.Errorf("peak tokens = %d, want 160000 (high-water mark)", tr.Tokens())
	}
	if got := tr.Occupancy(); math.Abs(got-0.8) > 1e-9 {
		t.Errorf("final occupancy = %v, want 0.8", got)
	}
}

// TestTrackerTrips asserts the soft/hard tiers fire at the configured thresholds
// and stay tripped once reached.
func TestTrackerTrips(t *testing.T) {
	tr := NewTracker(200000, 0.75, 0.90)

	// Below soft.
	feed(t, tr, []string{deltaLine(140000)}, nil) // 0.70
	if tr.Trip() != TripNone || tr.SoftTripped() {
		t.Fatalf("at 70%% want TripNone, got %v", tr.Trip())
	}

	// Cross soft (exactly 75%).
	feed(t, tr, []string{deltaLine(150000)}, nil) // 0.75
	if tr.Trip() != TripSoft {
		t.Fatalf("at 75%% want TripSoft, got %v", tr.Trip())
	}
	if !tr.SoftTripped() || tr.HardTripped() {
		t.Errorf("at 75%% want soft=true hard=false, got soft=%v hard=%v", tr.SoftTripped(), tr.HardTripped())
	}

	// Cross hard (exactly 90%).
	feed(t, tr, []string{deltaLine(180000)}, nil) // 0.90
	if tr.Trip() != TripHard {
		t.Fatalf("at 90%% want TripHard, got %v", tr.Trip())
	}
	if !tr.SoftTripped() || !tr.HardTripped() {
		t.Errorf("at 90%% both tiers should read tripped, got soft=%v hard=%v", tr.SoftTripped(), tr.HardTripped())
	}

	// A later dip must NOT un-trip (monotonic guardrail decision).
	feed(t, tr, []string{deltaLine(10000)}, nil)
	if tr.Trip() != TripHard {
		t.Errorf("a dip must not un-trip: got %v, want TripHard", tr.Trip())
	}
}

// TestTrackerLeadOnly: subagent usage must never inflate the lead's pressure.
func TestTrackerLeadOnly(t *testing.T) {
	tr := NewTracker(200000, 0.75, 0.90)
	feed(t, tr, []string{
		deltaLine(40000), // lead: 20%
		subLine(190000),  // subagent: would be 95% if (wrongly) counted
	}, nil)
	if tr.Tokens() != 40000 {
		t.Errorf("subagent usage leaked into the lead peak: tokens = %d, want 40000", tr.Tokens())
	}
	if tr.Trip() != TripNone {
		t.Errorf("subagent usage tripped the lead guardrail: %v", tr.Trip())
	}
}

// TestTrackerWindowAdopt: with window_tokens = 0 the live % is unavailable (and
// the guardrail stays quiet) until a result event reports the model's window, at
// which point occupancy resolves — specs/08 §context window.
func TestTrackerWindowAdopt(t *testing.T) {
	tr := NewTracker(0, 0.75, 0.90)
	feed(t, tr, []string{deltaLine(150000)}, func(occ float64) {
		if occ != 0 {
			t.Errorf("occupancy with unknown window = %v, want 0", occ)
		}
	})
	if tr.Trip() != TripNone {
		t.Fatalf("no window ⇒ TripNone, got %v", tr.Trip())
	}
	resultLine := `{"type":"result","subtype":"success","modelUsage":{"claude-opus-4-7":{"contextWindow":200000}}}`
	feed(t, tr, []string{resultLine}, nil)
	if tr.Window() != 200000 {
		t.Fatalf("window not adopted from result: %d", tr.Window())
	}
	if got := tr.Occupancy(); math.Abs(got-0.75) > 1e-9 {
		t.Errorf("occupancy after window adopt = %v, want 0.75", got)
	}
	if tr.Trip() != TripSoft {
		t.Errorf("after adopting window the 75%% peak should be TripSoft, got %v", tr.Trip())
	}
}

// TestTrackerDefaultsAndZeroValue: NewTracker fills missing thresholds with the
// spec defaults, and a bare zero-value Tracker (no window) never trips.
func TestTrackerDefaultsAndZeroValue(t *testing.T) {
	tr := NewTracker(200000, 0, 0) // 0/0 → defaults
	feed(t, tr, []string{deltaLine(150000)}, nil)
	if tr.Trip() != TripSoft {
		t.Errorf("default soft 0.75 should trip at 75%%, got %v", tr.Trip())
	}
	feed(t, tr, []string{deltaLine(180000)}, nil)
	if tr.Trip() != TripHard {
		t.Errorf("default hard 0.90 should trip at 90%%, got %v", tr.Trip())
	}

	var zero Tracker // window 0, pct 0 — must be inert, not a spurious hard trip
	zero.Update(mustEvent(t, deltaLine(999999)))
	if zero.Trip() != TripNone || zero.Occupancy() != 0 {
		t.Errorf("zero-value Tracker must be inert, got trip=%v occ=%v", zero.Trip(), zero.Occupancy())
	}
}

// TestTrackerMatchesObservation is the anti-drift lock: driven over the same real
// transcript through ObserveFunc, the live Tracker's peak must equal the post-hoc
// LoopObservation.PeakLeadTokens — proof both fold the identical lead usage
// (leadUsage is the single source of truth).
func TestTrackerMatchesObservation(t *testing.T) {
	tr := NewTracker(200000, 0.75, 0.90)
	obs, err := ObserveFunc(openFixture(t, "basic.jsonl"), nil, tr.Update)
	if err != nil {
		t.Fatalf("ObserveFunc: %v", err)
	}
	if tr.Tokens() != obs.PeakLeadTokens {
		t.Errorf("Tracker peak %d != observation PeakLeadTokens %d", tr.Tokens(), obs.PeakLeadTokens)
	}
	if math.Abs(tr.Occupancy()-obs.Occupancy(200000)) > 1e-12 {
		t.Errorf("Tracker occupancy %v != observation %v", tr.Occupancy(), obs.Occupancy(200000))
	}
}

func mustEvent(t *testing.T, line string) *Event {
	t.Helper()
	d := NewDecoder(strings.NewReader(line), nil)
	ev, err := d.Next()
	if err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	return ev
}
