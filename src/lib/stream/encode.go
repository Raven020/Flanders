package stream

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// EncodeUserMessage builds one outbound stream-json line: a user turn to write to
// the `claude` CLI's stdin under `--input-format stream-json`. It is the inbound
// decoder's counterpart — the single place that knows the wire shape in BOTH
// directions, so no consumer hand-assembles JSON.
//
// Two consumers need it (specs/01 §Guardrails, specs/05 §discuss):
//   - the soft wind-down (3.11 tier 2) injects a "wrap up" user message into a
//     running loop session, and
//   - discuss (Phase 7) injects each operator turn into a long-lived session.
//
// WIRE SHAPE — OPEN, best-effort. specs/08 §OPEN records that the *outbound*
// envelope has NOT been captured from a real CLI transcript (only the output side
// is pinned by the testdata fixtures). The shape below mirrors the Anthropic
// Messages API user turn (`{"type":"user","message":{"role":"user","content":[…]}}`),
// which is what the inbound decoder already parses for a [TypeUser] event — so the
// harness's own decoder round-trips it (asserted by the encode test). Re-verify and
// tighten against a captured input transcript when one is available, exactly as the
// usage-limit phrasing (task 2.3) is pending a real limited transcript. The trailing
// newline makes it a complete NDJSON line ready to write to stdin.
func EncodeUserMessage(text string) ([]byte, error) {
	msg := outboundUser{Type: TypeUser}
	msg.Message.Role = "user"
	msg.Message.Content = []outboundBlock{{Type: "text", Text: text}}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep prompt text byte-faithful (no &<> escaping)
	if err := enc.Encode(msg); err != nil {
		return nil, fmt.Errorf("stream: encode user message: %w", err)
	}
	// json.Encoder.Encode already appends a newline, so buf is a complete line.
	return buf.Bytes(), nil
}

// outboundUser / outboundBlock are the minimal user-turn envelope. They are kept
// separate from the rich inbound types (APIMessage/ContentBlock) on purpose: those
// carry many decode-only fields (usage, stop_reason, tool_result siblings) that
// must NOT be emitted on the way out.
type outboundUser struct {
	Type    string `json:"type"`
	Message struct {
		Role    string          `json:"role"`
		Content []outboundBlock `json:"content"`
	} `json:"message"`
}

type outboundBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
