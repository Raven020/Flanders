// Package stream decodes the newline-delimited JSON ("stream-json") output of
// the `claude` CLI (target 2.1.x) into typed Go events and folds them into a
// derived [LoopObservation].
//
// Why this package exists — single source of truth. The TUI, the journal, and
// the loop's guardrails all need to understand the agent's output. Rather than
// have each consumer re-parse the wire bytes, the harness decodes the stream
// exactly once here into typed values; every consumer reads those. This is the
// rule from specs/08-stream-json-protocol.md ("no ad-hoc re-parsing per
// consumer").
//
// Wire shapes are PINNED against a real `claude 2.1.150` transcript captured
// with the exact flags the harness uses:
//
//	claude -p --output-format stream-json --verbose --include-partial-messages
//
// and stored verbatim under testdata/ as the fixtures the tests assert against.
// Spec 08 originally only *inferred* these shapes (every wire detail marked
// OPEN); the captured fixtures are now the ground truth. Notable differences the
// capture revealed versus the draft spec — all reflected in the types below:
//
//   - A dedicated "rate_limit_event" carries usage-window state out-of-band, with
//     a clean epoch "resetsAt" — the spec had assumed the reset time would have to
//     be text-scraped out of an error result. See [RateLimitInfo].
//   - "message_delta" carries the FULL cumulative usage object (input + cache +
//     output), not just output_tokens. See [Usage].
//   - "result" reports per-model usage including the model's "contextWindow", so
//     the CLI itself answers the model→window question. See [ModelUsage].
//   - Subagent spawns are "tool_use" blocks named "Task"/"Agent" whose input
//     carries "subagent_type" (the agent class), not a model/effort. Inner events
//     of a subagent carry a non-empty "parent_tool_use_id". See [Event].
//
// Resilience (specs/08 §rules): a line that fails to parse is logged and skipped
// — it must never crash the loop. An unknown top-level "type" (or an unknown
// content block) is preserved verbatim in [Event.Raw] and ignored by feature
// logic, so the harness stays forward-compatible with future CLI versions.
package stream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"time"
)

// Top-level event types the harness understands. Any other type string is an
// "unknown" event: its [Event.Raw] is preserved for the journal and its payload
// pointers are nil.
const (
	TypeSystem      = "system"           // session lifecycle; subtype "init" carries model/tools/perm
	TypeStreamEvent = "stream_event"     // raw Anthropic streaming event (only with --include-partial-messages)
	TypeAssistant   = "assistant"        // a complete assistant message (assembled content blocks)
	TypeUser        = "user"             // a user message (tool_result blocks)
	TypeResult      = "result"           // terminal event, once per session
	TypeRateLimit   = "rate_limit_event" // subscription usage-window state (out-of-band)
)

// Event is one decoded NDJSON line. The common envelope fields are always
// populated; exactly one typed payload pointer is set for a known [TypeSystem]…
// [TypeRateLimit] line (or none, for an unknown type). Raw holds the verbatim
// line bytes so the journal can archive — and a future CLI can extend — the wire
// format without this package losing data.
type Event struct {
	Type            string // see the Type* constants; any other value ⇒ unknown
	Subtype         string // system: "init"|"status"; result: "success"|"error_*"
	SessionID       string
	UUID            string
	ParentToolUseID string // non-empty ⇒ this event is a subagent's inner activity

	Raw json.RawMessage // the verbatim line (journal + forward-compat)

	System      *SystemEvent
	StreamEvent *StreamEvent
	Assistant   *APIMessage // when Type == TypeAssistant
	User        *APIMessage // when Type == TypeUser (tool_result blocks)
	Result      *ResultEvent
	RateLimit   *RateLimitInfo
}

// SystemEvent is the payload of a "system" line. The "init" subtype confirms the
// session and reports the resolved model, permission mode, and tool set.
type SystemEvent struct {
	Subtype        string   `json:"subtype"`
	Model          string   `json:"model"`
	PermissionMode string   `json:"permissionMode"`
	Tools          []string `json:"tools"`
	CWD            string   `json:"cwd"`
}

// Usage is the Anthropic token-usage object as it appears on stream_event
// sub-events, assistant messages, and the final result. All four fields count
// toward context occupancy; the spec says to over-count (include cache) so the
// pressure guardrail trips early rather than late.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// Total is the context-occupancy sum: every token category the turn is carrying.
func (u Usage) Total() int {
	return u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

// StreamEvent mirrors the raw Anthropic streaming "event" nested under a
// stream_event line. Its sub-type (message_start, content_block_delta, …) lives
// in Type. The harness uses these primarily for LIVE token tracking; complete
// text/tool_use blocks are read off the assembled [TypeAssistant] messages
// instead, which is far simpler than reassembling deltas.
type StreamEvent struct {
	Type         string        `json:"type"`          // message_start|content_block_start|content_block_delta|content_block_stop|message_delta|message_stop
	Index        int           `json:"index"`         // content-block index for block_* events
	Message      *APIMessage   `json:"message"`       // message_start: event.message (carries .usage)
	Usage        *Usage        `json:"usage"`         // message_delta: cumulative usage for the turn
	Delta        *Delta        `json:"delta"`         // content_block_delta / message_delta
	ContentBlock *ContentBlock `json:"content_block"` // content_block_start
}

// LiveUsage returns the most complete usage object this stream event carries, or
// nil. message_delta puts usage at event.usage; message_start nests it under
// event.message.usage.
func (s *StreamEvent) LiveUsage() *Usage {
	if s.Usage != nil {
		return s.Usage
	}
	if s.Message != nil {
		return s.Message.Usage
	}
	return nil
}

// Delta is the incremental payload of a content_block_delta or message_delta.
type Delta struct {
	Type        string `json:"type"`         // text_delta|thinking_delta|input_json_delta|signature_delta
	Text        string `json:"text"`         // text_delta
	Thinking    string `json:"thinking"`     // thinking_delta
	PartialJSON string `json:"partial_json"` // input_json_delta (tool_use args streamed)
	StopReason  string `json:"stop_reason"`  // message_delta
}

// APIMessage is the Anthropic Messages object carried by assistant/user lines and
// by stream_event/message_start. For an assistant message Content holds the
// assembled text/thinking/tool_use blocks; for a user message it holds
// tool_result blocks.
type APIMessage struct {
	Role       string         `json:"role"`
	Model      string         `json:"model"`
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      *Usage         `json:"usage"`
}

// ContentBlock is a single content block. The relevant fields depend on Type:
// "text"/"thinking" use Text/Thinking; "tool_use" uses ID/Name/Input; and
// "tool_result" (in a user message) uses ToolUseID/Content/IsError.
type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"`

	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// ResultText extracts the human-readable text of a tool_result block. The wire
// "content" is either a bare string or an array of {type,text} parts.
func (b ContentBlock) ResultText() string {
	if len(b.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(b.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(b.Content, &parts) == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return string(b.Content)
}

// ResultEvent is the terminal "result" line, emitted once per session. It is the
// authoritative source for cost, final token totals, the outcome (subtype /
// is_error), and — via ModelUsage — the model's context window.
type ResultEvent struct {
	Subtype        string                `json:"subtype"`          // "success" | "error_max_turns" | "error_during_execution" | …
	IsError        bool                  `json:"is_error"`
	APIErrorStatus string                `json:"api_error_status"` // non-empty on an API-level error (null on success)
	Result         string                `json:"result"`           // final assistant text, or the error message
	StopReason     string                `json:"stop_reason"`
	TerminalReason string                `json:"terminal_reason"`
	TotalCostUSD   float64               `json:"total_cost_usd"`   // info/throughput only — never a stop condition
	DurationMS     int64                 `json:"duration_ms"`
	NumTurns       int                   `json:"num_turns"`
	SessionID      string                `json:"session_id"`
	Usage          *Usage                `json:"usage"`
	ModelUsage     map[string]ModelUsage `json:"modelUsage"`
}

// ModelUsage is the per-model breakdown in a result event. ContextWindow is the
// model's window size as reported by the CLI itself — the harness can use it to
// turn token counts into an occupancy % without maintaining a model→window table.
type ModelUsage struct {
	InputTokens             int     `json:"inputTokens"`
	OutputTokens            int     `json:"outputTokens"`
	CacheReadInputTokens    int     `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int    `json:"cacheCreationInputTokens"`
	CostUSD                 float64 `json:"costUSD"`
	ContextWindow           int     `json:"contextWindow"`
	MaxOutputTokens         int     `json:"maxOutputTokens"`
}

// RateLimitInfo is the payload of a "rate_limit_event" — the subscription
// usage-window state, reported out-of-band (not only on error). This is the clean
// signal for the usage-wait guardrail (specs/01 §usage limits, task 3.12):
// ResetsAt is a Unix epoch, so no fragile text-scraping is needed.
type RateLimitInfo struct {
	Status                string `json:"status"`                // observed: "allowed"; limited values are OPEN (see spec 08)
	ResetsAt              int64  `json:"resetsAt"`              // Unix epoch seconds of the next window reset
	RateLimitType         string `json:"rateLimitType"`         // observed: "five_hour"
	OverageStatus         string `json:"overageStatus"`
	OverageDisabledReason string `json:"overageDisabledReason"`
	IsUsingOverage        bool   `json:"isUsingOverage"`
}

// ResetTime converts ResetsAt to a time.Time (zero if unset).
func (r RateLimitInfo) ResetTime() time.Time {
	if r.ResetsAt <= 0 {
		return time.Time{}
	}
	return time.Unix(r.ResetsAt, 0).UTC()
}

// Limited reports whether the window is exhausted (status present and not
// "allowed"). The exact non-allowed wording is OPEN until a real limited
// transcript is captured; treating anything other than "allowed"/"" as limited
// errs toward pausing rather than aborting an unattended run.
func (r RateLimitInfo) Limited() bool {
	return r.Status != "" && r.Status != "allowed"
}

// Decoder reads stream-json lines from an io.Reader and yields typed [Event]s.
// It is line-oriented (so a single malformed line can be skipped without losing
// stream sync) and imposes no per-line length limit (tool inputs, thinking
// signatures, and file contents can be large).
type Decoder struct {
	r   *bufio.Reader
	log *slog.Logger

	Lines   int // events successfully decoded and returned
	Skipped int // lines skipped because they were not valid JSON
}

// NewDecoder wraps r. A nil log discards skip diagnostics.
func NewDecoder(r io.Reader, log *slog.Logger) *Decoder {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Decoder{r: bufio.NewReaderSize(r, 64*1024), log: log}
}

// Next returns the next event. Lines that are not valid JSON are logged and
// skipped (never fatal). The error is io.EOF once the stream is exhausted.
func (d *Decoder) Next() (*Event, error) {
	for {
		line, err := d.r.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			ev, perr := decodeLine(trimmed, d.log)
			if perr == nil {
				d.Lines++
				return ev, nil
			}
			d.Skipped++
			d.log.Warn("stream: skipping unparseable line", "err", perr, "bytes", len(trimmed))
		}
		if err != nil {
			return nil, err // io.EOF at clean end, or a real read error
		}
	}
}

// Stream runs a [Decoder] in a goroutine and delivers events on a channel,
// matching the spec's "emit typed events on a channel from a raw io.Reader"
// shape. The channel is closed at EOF, on read error, or when ctx is cancelled.
func Stream(ctx context.Context, r io.Reader, log *slog.Logger) <-chan *Event {
	ch := make(chan *Event)
	go func() {
		defer close(ch)
		d := NewDecoder(r, log)
		for {
			ev, err := d.Next()
			if err != nil {
				return
			}
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// decodeLine parses one trimmed, non-empty line. It first decodes the common
// envelope (so an unknown type still yields a usable Event with Raw preserved),
// then decodes the type-specific payload. A payload-level decode failure is
// logged but non-fatal: the envelope Event is still returned with Raw intact, so
// the journal never loses the line.
func decodeLine(b []byte, log *slog.Logger) (*Event, error) {
	var env struct {
		Type            string `json:"type"`
		Subtype         string `json:"subtype"`
		SessionID       string `json:"session_id"`
		UUID            string `json:"uuid"`
		ParentToolUseID string `json:"parent_tool_use_id"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, err
	}

	raw := make(json.RawMessage, len(b))
	copy(raw, b)
	ev := &Event{
		Type:            env.Type,
		Subtype:         env.Subtype,
		SessionID:       env.SessionID,
		UUID:            env.UUID,
		ParentToolUseID: env.ParentToolUseID,
		Raw:             raw,
	}

	var perr error
	switch env.Type {
	case TypeSystem:
		var s SystemEvent
		if perr = json.Unmarshal(b, &s); perr == nil {
			ev.System = &s
		}
	case TypeStreamEvent:
		var w struct {
			Event StreamEvent `json:"event"`
		}
		if perr = json.Unmarshal(b, &w); perr == nil {
			ev.StreamEvent = &w.Event
		}
	case TypeAssistant:
		var w struct {
			Message APIMessage `json:"message"`
		}
		if perr = json.Unmarshal(b, &w); perr == nil {
			ev.Assistant = &w.Message
		}
	case TypeUser:
		var w struct {
			Message APIMessage `json:"message"`
		}
		if perr = json.Unmarshal(b, &w); perr == nil {
			ev.User = &w.Message
		}
	case TypeResult:
		var r ResultEvent
		if perr = json.Unmarshal(b, &r); perr == nil {
			ev.Result = &r
		}
	case TypeRateLimit:
		var w struct {
			Info RateLimitInfo `json:"rate_limit_info"`
		}
		if perr = json.Unmarshal(b, &w); perr == nil {
			ev.RateLimit = &w.Info
		}
	default:
		// Unknown type: Raw is preserved, payloads stay nil (forward-compatible).
	}
	if perr != nil {
		// Known type but the payload didn't fit our struct — keep the envelope
		// event (Raw intact) so nothing is lost; just note it.
		log.Warn("stream: payload decode failed; keeping raw envelope", "type", env.Type, "err", perr)
	}
	return ev, nil
}
