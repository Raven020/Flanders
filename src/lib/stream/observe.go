package stream

import (
	"encoding/json"
	"io"
	"log/slog"
	"time"
)

// LoopObservation is the derived, single-pass summary of one loop's agent output
// — what the loop driver (Phase 3), the journal, and the guardrails act on. It is
// the "one typed stream, one source of truth" the spec mandates: produced once
// from the event stream, never re-parsed per consumer.
type LoopObservation struct {
	SessionID      string // from system/init (or the result event)
	Model          string // resolved model, from system/init
	PermissionMode string // resolved permission mode, from system/init

	Texts     []string        // lead-agent assistant text blocks, in order
	ToolCalls []ToolCall       // every tool_use seen (lead and subagent; see ToolCall.Parent)
	Subagents []SubagentSpawn // tool_use blocks named Task/Agent — the agent tree

	// Tokens & context. PeakLeadTokens is the high-water mark of the LEAD agent's
	// context occupancy (subagent usage is excluded on purpose: a subagent's
	// context does not count against the main loop's window — specs/01). FinalUsage
	// is the result event's billing total, which DOES include subagents.
	PeakLeadTokens int
	FinalUsage     Usage
	ContextWindow  int     // model's window, as reported by the CLI (0 if unknown)
	Cost           float64 // total_cost_usd — info/throughput only
	DurationMS     int64
	NumTurns       int

	// Terminal outcome (set once the result event arrives).
	Done           bool
	Subtype        string // "success" | "error_*"
	IsError        bool
	ResultText     string
	APIErrorStatus string // non-empty ⇒ API-level error; a classification hint for usage-limit detection (2.3)

	// Usage-window state. UsageLimited is true once a subscription usage/rate
	// window is exhausted — set from EITHER an out-of-band rate_limit_event OR a
	// usage-limit `result` (api_error_status 429 / "usage limit reached" text; see
	// [LoopObservation.Classify]). ResetAt is the parsed window reset (nil if never
	// reported): the rate_limit_event epoch when present, else an epoch parsed from
	// the result text. The usage-wait guardrail (3.12) consumes these and falls
	// back to [usage].backoff when ResetAt is nil.
	UsageLimited  bool
	ResetAt       *time.Time
	RateLimitType string

	Lines   int // total lines decoded
	Skipped int // lines skipped as unparseable
}

// ToolCall records one tool_use block. Parent is the parent_tool_use_id of the
// event it appeared in: empty for a lead-agent call, set when a subagent made the
// call. IsError/Result are filled in from the matching tool_result, when present.
type ToolCall struct {
	ID      string
	Name    string
	Input   json.RawMessage
	Parent  string
	IsError bool
	Result  string
}

// SubagentSpawn records a Task/Agent tool_use — a delegated subagent. The wire
// carries the agent class (SubagentType), not a model/effort; the harness resolves
// those from [subagents] config (specs/07).
type SubagentSpawn struct {
	ToolUseID    string
	SubagentType string // e.g. "general-purpose", "Explore"
	Description  string
}

// Occupancy returns the lead agent's peak context occupancy as a fraction of the
// window. It prefers the explicit window argument (config's [context].window_tokens);
// when that is 0 it falls back to the window the CLI reported. Returns 0 if no
// window is known.
func (o *LoopObservation) Occupancy(window int) float64 {
	if window <= 0 {
		window = o.ContextWindow
	}
	if window <= 0 {
		return 0
	}
	return float64(o.PeakLeadTokens) / float64(window)
}

// Observe decodes the entire stream and returns the folded observation. Read
// errors (other than EOF) are returned along with the partial observation.
func Observe(r io.Reader, log *slog.Logger) (*LoopObservation, error) {
	return ObserveFunc(r, log, nil)
}

// ObserveFunc is Observe with a per-event hook. The hook sees every decoded event
// before it is folded — the single seam for consumers (the journal archiving raw
// bytes, the live token meter in the TUI) that need the events as they arrive,
// without re-decoding the stream.
func ObserveFunc(r io.Reader, log *slog.Logger, onEvent func(*Event)) (*LoopObservation, error) {
	d := NewDecoder(r, log)
	obs := &LoopObservation{}
	results := map[string]toolResult{} // tool_use_id → outcome, applied after the stream

	var readErr error
	for {
		ev, err := d.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			readErr = err
			break
		}
		if onEvent != nil {
			onEvent(ev)
		}
		obs.fold(ev, results)
	}

	// Reconcile tool results into their calls after the fact: doing it during the
	// fold would mean holding pointers into ToolCalls, which append() invalidates.
	for i := range obs.ToolCalls {
		if tr, ok := results[obs.ToolCalls[i].ID]; ok {
			obs.ToolCalls[i].IsError = tr.isError
			obs.ToolCalls[i].Result = tr.text
		}
	}
	obs.Lines = d.Lines
	obs.Skipped = d.Skipped
	return obs, readErr
}

type toolResult struct {
	isError bool
	text    string
}

// fold accumulates one event into the observation.
func (o *LoopObservation) fold(ev *Event, results map[string]toolResult) {
	lead := ev.ParentToolUseID == "" // lead-agent (not a subagent's inner activity)

	// Context occupancy: advance the lead-agent token high-water mark. leadUsage is
	// the single source of truth for "what counts as lead context" — the live
	// [Tracker] folds the very same usage, so the two can never disagree.
	if tok, ok := leadUsage(ev); ok {
		o.trackPeak(tok)
	}

	switch ev.Type {
	case TypeSystem:
		if o.SessionID == "" && ev.SessionID != "" {
			o.SessionID = ev.SessionID
		}
		if ev.System != nil && ev.System.Subtype == "init" {
			if ev.System.Model != "" {
				o.Model = ev.System.Model
			}
			if ev.System.PermissionMode != "" {
				o.PermissionMode = ev.System.PermissionMode
			}
		}

	case TypeAssistant:
		if ev.Assistant == nil {
			return
		}
		for _, b := range ev.Assistant.Content {
			switch b.Type {
			case "text":
				if lead && b.Text != "" {
					o.Texts = append(o.Texts, b.Text)
				}
			case "tool_use":
				o.ToolCalls = append(o.ToolCalls, ToolCall{
					ID:     b.ID,
					Name:   b.Name,
					Input:  b.Input,
					Parent: ev.ParentToolUseID,
				})
				if b.Name == "Task" || b.Name == "Agent" {
					var in struct {
						SubagentType string `json:"subagent_type"`
						Description  string `json:"description"`
					}
					_ = json.Unmarshal(b.Input, &in)
					o.Subagents = append(o.Subagents, SubagentSpawn{
						ToolUseID:    b.ID,
						SubagentType: in.SubagentType,
						Description:  in.Description,
					})
				}
			}
		}

	case TypeUser:
		if ev.User == nil {
			return
		}
		for _, b := range ev.User.Content {
			if b.Type == "tool_result" {
				results[b.ToolUseID] = toolResult{isError: b.IsError, text: b.ResultText()}
			}
		}

	case TypeResult:
		if ev.Result == nil {
			return
		}
		r := ev.Result
		o.Done = true
		o.Subtype = r.Subtype
		o.IsError = r.IsError
		o.APIErrorStatus = r.APIErrorStatus
		o.ResultText = r.Result
		o.Cost = r.TotalCostUSD
		o.DurationMS = r.DurationMS
		o.NumTurns = r.NumTurns
		if r.Usage != nil {
			o.FinalUsage = *r.Usage
		}
		if r.SessionID != "" {
			o.SessionID = r.SessionID
		}
		// The window the CLI reports for the lead model: take the largest, since a
		// loop may briefly use a smaller helper model with a different window.
		for _, mu := range r.ModelUsage {
			if mu.ContextWindow > o.ContextWindow {
				o.ContextWindow = mu.ContextWindow
			}
		}
		// A usage-limit can surface only as an error result (no out-of-band
		// rate_limit_event). Detect it here so UsageLimited is comprehensive and the
		// reset is as precise as the wire allows — without overwriting a cleaner
		// epoch the rate_limit_event already supplied. See classify.go.
		if isUsageLimitResult(r.APIErrorStatus, r.Result) {
			o.UsageLimited = true
			if o.ResetAt == nil {
				if t, ok := parseResetFromText(r.Result); ok {
					o.ResetAt = &t
				}
			}
		}

	case TypeRateLimit:
		if ev.RateLimit == nil {
			return
		}
		ri := ev.RateLimit
		if ri.RateLimitType != "" {
			o.RateLimitType = ri.RateLimitType
		}
		if t := ri.ResetTime(); !t.IsZero() {
			o.ResetAt = &t
		}
		if ri.Limited() {
			o.UsageLimited = true
		}
	}
}

func (o *LoopObservation) trackPeak(total int) {
	if total > o.PeakLeadTokens {
		o.PeakLeadTokens = total
	}
}

// leadUsage returns the context-occupancy token total carried by ev, and whether
// ev is a LEAD-agent event that carries usage at all. It is the one place the
// harness decides "what counts toward the lead's context window," shared by the
// post-hoc [LoopObservation.fold] and the live [Tracker] so they cannot drift.
//
// Two rules from specs/08:
//   - Lead-only: subagent inner events (non-empty parent_tool_use_id) carry usage
//     for their OWN context and are excluded — a subagent must not inflate the
//     lead's pressure %.
//   - Most-complete usage wins: a stream_event prefers message_delta's cumulative
//     usage over message_start's (via StreamEvent.LiveUsage); an assistant message
//     carries the assembled turn's usage. Total() over-counts cache categories on
//     purpose, so the pressure guardrail trips early rather than late.
func leadUsage(ev *Event) (int, bool) {
	if ev.ParentToolUseID != "" {
		return 0, false
	}
	switch ev.Type {
	case TypeStreamEvent:
		if ev.StreamEvent != nil {
			if u := ev.StreamEvent.LiveUsage(); u != nil {
				return u.Total(), true
			}
		}
	case TypeAssistant:
		if ev.Assistant != nil && ev.Assistant.Usage != nil {
			return ev.Assistant.Usage.Total(), true
		}
	}
	return 0, false
}
