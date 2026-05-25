package stream

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestEncodeUserMessageShape(t *testing.T) {
	line, err := EncodeUserMessage("wrap up now: mark blocked, write handoff, commit, end")
	if err != nil {
		t.Fatalf("EncodeUserMessage: %v", err)
	}
	if !bytes.HasSuffix(line, []byte("\n")) {
		t.Fatalf("encoded line must end in a newline (complete NDJSON line): %q", line)
	}
	if bytes.Count(line, []byte("\n")) != 1 {
		t.Fatalf("encoded user message must be exactly one line, got %q", line)
	}

	var got struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(line), &got); err != nil {
		t.Fatalf("encoded line is not valid JSON: %v", err)
	}
	if got.Type != TypeUser {
		t.Errorf("type = %q, want %q", got.Type, TypeUser)
	}
	if got.Message.Role != "user" {
		t.Errorf("role = %q, want user", got.Message.Role)
	}
	if len(got.Message.Content) != 1 || got.Message.Content[0].Type != "text" {
		t.Fatalf("content = %+v, want one text block", got.Message.Content)
	}
	if want := "wrap up now: mark blocked, write handoff, commit, end"; got.Message.Content[0].Text != want {
		t.Errorf("text = %q, want %q", got.Message.Content[0].Text, want)
	}
}

// The encoder's whole justification is that the harness's own inbound decoder
// understands what it emits — so a soft-wind-down message is a real user turn, not
// a malformed line the CLI would skip. Round-trip it through the Decoder.
func TestEncodeUserMessageRoundTripsThroughDecoder(t *testing.T) {
	line, err := EncodeUserMessage("hello")
	if err != nil {
		t.Fatalf("EncodeUserMessage: %v", err)
	}
	d := NewDecoder(bytes.NewReader(line), nil)
	ev, err := d.Next()
	if err != nil {
		t.Fatalf("decode emitted line: %v", err)
	}
	if ev.Type != TypeUser {
		t.Fatalf("decoded type = %q, want %q", ev.Type, TypeUser)
	}
	if ev.User == nil || len(ev.User.Content) != 1 || ev.User.Content[0].Text != "hello" {
		t.Fatalf("decoded user message did not carry the text: %+v", ev.User)
	}
}

// HTML escaping is off so prompt text stays byte-faithful (a task often contains
// <, >, &). Verify those survive unescaped.
func TestEncodeUserMessageNoHTMLEscape(t *testing.T) {
	line, err := EncodeUserMessage("if a < b && c > d, fix it")
	if err != nil {
		t.Fatalf("EncodeUserMessage: %v", err)
	}
	if !strings.Contains(string(line), "a < b && c > d") {
		t.Errorf("prompt punctuation was escaped: %s", line)
	}
}
