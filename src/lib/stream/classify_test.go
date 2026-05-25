package stream

import (
	"strings"
	"testing"
)

// observe is a small helper: fold a synthetic NDJSON transcript. Usage-limit
// transcripts are SYNTHETIC (inline, clearly labelled) — unlike the *.jsonl
// fixtures, which are verbatim real captures — because a real subscription limit
// could not be triggered to capture (spec 08 OPEN). The wire shapes (result
// is_error / api_error_status / "usage limit reached|<epoch>") follow the
// historical CLI behaviour the spec pins as best-effort.
func observe(t *testing.T, lines ...string) *LoopObservation {
	t.Helper()
	obs, err := Observe(strings.NewReader(strings.Join(lines, "\n")), nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return obs
}

// A clean success is OutcomeSuccess on a zero exit; a non-zero exit on an
// otherwise-clean run is a process-level error (kill/crash), not success.
func TestClassifySuccess(t *testing.T) {
	obs := observe(t, `{"type":"result","subtype":"success","is_error":false,"result":"all good","session_id":"s1"}`)
	if obs.UsageLimited {
		t.Fatal("a clean success must not be UsageLimited")
	}
	if got := obs.Classify(0); got != OutcomeSuccess {
		t.Errorf("Classify(0) = %v, want success", got)
	}
	if got := obs.Classify(2); got != OutcomeError {
		t.Errorf("Classify(2) = %v, want error (non-zero exit on clean run)", got)
	}
}

// A genuine agent/API error: is_error with a non-429 status and no limit text.
func TestClassifyGenuineError(t *testing.T) {
	obs := observe(t, `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":"500","result":"internal error in tool execution","session_id":"s1"}`)
	if obs.UsageLimited {
		t.Fatal("a 500 error must not be classified as a usage limit")
	}
	if got := obs.Classify(0); got != OutcomeError {
		t.Errorf("Classify(0) = %v, want error", got)
	}
	if got := obs.Classify(1); got != OutcomeError {
		t.Errorf("Classify(1) = %v, want error", got)
	}
}

// The "Claude AI usage limit reached|<epoch>" result form: detected as a limit,
// the trailing epoch parsed into ResetAt, and classified UsageLimit even though it
// arrives as is_error with a non-zero exit (the precedence rule).
func TestClassifyUsageLimitFromResultText(t *testing.T) {
	obs := observe(t, `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Claude AI usage limit reached|1779691800","session_id":"s1"}`)
	if !obs.UsageLimited {
		t.Fatal("UsageLimited should be set from the result text")
	}
	if obs.ResetAt == nil || obs.ResetAt.Unix() != 1779691800 {
		t.Fatalf("ResetAt = %v, want epoch 1779691800 parsed from the text", obs.ResetAt)
	}
	if got := obs.Classify(0); got != OutcomeUsageLimit {
		t.Errorf("Classify(0) = %v, want usage_limit", got)
	}
	if got := obs.Classify(1); got != OutcomeUsageLimit {
		t.Errorf("Classify(1) = %v, want usage_limit (limit wins over error path)", got)
	}
}

// An API 429 with no limit phrase in the text — the status alone is the signal.
// No epoch is available, so ResetAt stays nil and the caller falls back to backoff.
func TestClassifyUsageLimitFromAPI429(t *testing.T) {
	obs := observe(t, `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":"429","result":"request failed","session_id":"s1"}`)
	if !obs.UsageLimited {
		t.Fatal("a 429 api_error_status should classify as a usage limit")
	}
	if obs.ResetAt != nil {
		t.Errorf("ResetAt = %v, want nil (no epoch in this payload)", obs.ResetAt)
	}
	if got := obs.Classify(0); got != OutcomeUsageLimit {
		t.Errorf("Classify(0) = %v, want usage_limit", got)
	}
}

// The out-of-band rate_limit_event with a non-allowed status wins even when the
// result itself is a clean success: the window state is reported separately.
func TestClassifyUsageLimitFromRateLimitEvent(t *testing.T) {
	obs := observe(t,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1779691800,"rateLimitType":"five_hour"},"session_id":"s1"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s1"}`,
	)
	if !obs.UsageLimited {
		t.Fatal("a non-allowed rate_limit_event status should set UsageLimited")
	}
	if obs.ResetAt == nil || obs.ResetAt.Unix() != 1779691800 {
		t.Fatalf("ResetAt = %v, want the event epoch", obs.ResetAt)
	}
	if got := obs.Classify(0); got != OutcomeUsageLimit {
		t.Errorf("Classify(0) = %v, want usage_limit", got)
	}
}

// The cleaner rate_limit_event epoch overrides a text-parsed reset regardless of
// event order: here the limit result is parsed first, then the event corrects it.
func TestRateLimitEventEpochPreferredOverText(t *testing.T) {
	obs := observe(t,
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Claude AI usage limit reached|1700000000","session_id":"s1"}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1779691800,"rateLimitType":"five_hour"},"session_id":"s1"}`,
	)
	if obs.ResetAt == nil || obs.ResetAt.Unix() != 1779691800 {
		t.Fatalf("ResetAt = %v, want the rate_limit_event epoch to win over the text epoch", obs.ResetAt)
	}
}

func TestParseResetFromText(t *testing.T) {
	cases := []struct {
		in    string
		want  int64
		found bool
	}{
		{"Claude AI usage limit reached|1779691800", 1779691800, true},
		{"x|1779691800\n", 1779691800, true},
		{"x| 1779691800 ", 1779691800, true},
		{"x|1779691800.", 1779691800, true},
		{"x|1779691800123", 1779691800, true}, // millisecond epoch tolerated
		{"no pipe here", 0, false},
		{"trailing pipe|", 0, false},
		{"x|abc", 0, false},
		{"1779691800|reached", 0, false}, // epoch must follow the final pipe
	}
	for _, c := range cases {
		got, ok := parseResetFromText(c.in)
		if ok != c.found {
			t.Errorf("parseResetFromText(%q) found = %v, want %v", c.in, ok, c.found)
			continue
		}
		if ok && got.Unix() != c.want {
			t.Errorf("parseResetFromText(%q) = %d, want %d", c.in, got.Unix(), c.want)
		}
	}
}

func TestOutcomeString(t *testing.T) {
	if OutcomeSuccess.String() != "success" || OutcomeUsageLimit.String() != "usage_limit" || OutcomeError.String() != "error" {
		t.Errorf("Outcome.String mismatch: %v %v %v", OutcomeSuccess, OutcomeUsageLimit, OutcomeError)
	}
}
