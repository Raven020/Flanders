package stream

import (
	"strconv"
	"strings"
	"time"
)

// Outcome is the terminal classification of one loop's `claude` invocation — the
// input the loop driver (Phase 3) and the usage-wait guardrail (3.12) branch on.
//
// It describes the *invocation's* outcome (did the run hit a usage window, error
// out, or complete cleanly), NOT task completion: done-ness is ground-truthed by
// the harness's test gate, never the agent's own result (specs/01 §done-detection).
// So OutcomeSuccess means "the CLI ran to a clean result," not "the task is done."
type Outcome int

const (
	OutcomeSuccess    Outcome = iota // ran to a clean result → take the verify/checkpoint path
	OutcomeUsageLimit                // subscription usage/rate window exhausted → wait until reset, auto-resume
	OutcomeError                     // a genuine agent/API/process error → guardrail/halt path
)

func (o Outcome) String() string {
	switch o {
	case OutcomeSuccess:
		return "success"
	case OutcomeUsageLimit:
		return "usage_limit"
	case OutcomeError:
		return "error"
	default:
		return "unknown"
	}
}

// Classify reduces a finished loop to one of three outcomes. exitCode is the
// `claude` process's exit status (pass 0 when it exited cleanly or the code is
// not yet known — e.g. before the Phase-2.5 supervisor wires it in).
//
// A usage-limit is checked FIRST and wins over the generic error path. A limit
// surfaces as an error `result` (is_error / api_error_status / non-zero exit), and
// misclassifying it as a genuine error would abort an unattended multi-day run —
// the failure mode spec 08 calls out explicitly. The limit signal is comprehensive
// (set in [LoopObservation.fold] from either the out-of-band rate_limit_event OR a
// usage-limit result), so Classify only reads o.UsageLimited.
//
// A bare non-zero exit with no error/limit signal (e.g. the process crashed or was
// killed on the per-iteration timeout) is a genuine error — the supervisor's
// non-zero exit is the signal, since a killed stream may never reach a result.
func (o *LoopObservation) Classify(exitCode int) Outcome {
	if o.UsageLimited {
		return OutcomeUsageLimit
	}
	if o.IsError || o.APIErrorStatus != "" || exitCode != 0 {
		return OutcomeError
	}
	return OutcomeSuccess
}

// usageLimitPhrases are lowercase substrings that mark a `result` event as a
// subscription usage/rate-limit rather than a genuine error. The clean, primary
// signal is the out-of-band rate_limit_event; these phrases are the FALLBACK for
// the path where a limit surfaces only as an error result.
//
// The exact wording is OPEN in spec 08 — no real limit could be triggered to
// capture it — so this matches the known historical phrasings (the
// "Claude AI usage limit reached|<epoch>" message) and the Anthropic API rate-limit
// error. Bias per spec: lean toward pausing (treat as a limit) rather than aborting
// an unattended run on an ambiguous limit-like signal; max_cycles caps any runaway.
var usageLimitPhrases = []string{
	"usage limit reached",
	"usage limit exceeded",
	"rate limit exceeded",
	"rate_limit_error",
	"too many requests",
}

// isUsageLimitResult reports whether a `result` event signals a usage/rate-limit.
// It inspects api_error_status (an API 429 is the strong signal) and the result
// text. Status 529 (overloaded) is deliberately NOT matched: that is transient
// server overload, not a subscription-window exhaustion.
func isUsageLimitResult(apiErrorStatus, text string) bool {
	hay := strings.ToLower(apiErrorStatus + "\n" + text)
	if strings.Contains(hay, "429") {
		return true
	}
	for _, p := range usageLimitPhrases {
		if strings.Contains(hay, p) {
			return true
		}
	}
	return false
}

// parseResetFromText extracts a usage-window reset time embedded in a usage-limit
// result message. The known form is "Claude AI usage limit reached|<epoch>", where
// <epoch> is Unix seconds following the final "|". Returns false when no trailing
// epoch is present.
//
// WHY parse this when rate_limit_event already carries a clean epoch: the
// result-only path (a CLI build or transcript that surfaces the limit solely as an
// error result, with no out-of-band rate_limit_event) still needs a reset; the
// embedded epoch is far more precise than the blind [usage].backoff fallback.
func parseResetFromText(s string) (time.Time, bool) {
	i := strings.LastIndexByte(s, '|')
	if i < 0 {
		return time.Time{}, false
	}
	field := strings.TrimSpace(s[i+1:])
	// Keep only the leading run of digits (tolerate trailing punctuation/newline).
	end := 0
	for end < len(field) && field[end] >= '0' && field[end] <= '9' {
		end++
	}
	if end == 0 {
		return time.Time{}, false
	}
	epoch, err := strconv.ParseInt(field[:end], 10, 64)
	if err != nil || epoch <= 0 {
		return time.Time{}, false
	}
	if epoch >= 1e12 { // tolerate a millisecond epoch
		epoch /= 1000
	}
	return time.Unix(epoch, 0).UTC(), true
}
