package stream

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openFixture opens a captured real `claude 2.1.150` transcript. These are the
// ground truth the parser is pinned against (see package doc).
func openFixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("open fixture %s: %v", name, err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func findTool(obs *LoopObservation, name string) *ToolCall {
	for i := range obs.ToolCalls {
		if obs.ToolCalls[i].Name == name {
			return &obs.ToolCalls[i]
		}
	}
	return nil
}

// TestObserveBasic is the spec-08 acceptance gate: a fixture-based test over a
// captured real transcript asserting text / tool_use / result / token-usage
// extraction (plus the rate-limit reset, which the capture revealed).
func TestObserveBasic(t *testing.T) {
	obs, err := Observe(openFixture(t, "basic.jsonl"), nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.Skipped != 0 {
		t.Errorf("clean transcript should skip 0 lines, skipped %d", obs.Skipped)
	}

	// --- system/init: model + permission mode ---
	if obs.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want claude-sonnet-4-6", obs.Model)
	}
	if obs.PermissionMode != "bypassPermissions" {
		t.Errorf("PermissionMode = %q, want bypassPermissions", obs.PermissionMode)
	}
	if obs.SessionID != "aca71ef5-bd56-4e92-9292-d0f4fda0a3e7" {
		t.Errorf("SessionID = %q", obs.SessionID)
	}

	// --- tool_use extraction: the Bash call and its assembled input ---
	bash := findTool(obs, "Bash")
	if bash == nil {
		t.Fatalf("no Bash tool_use found; got %d tool calls", len(obs.ToolCalls))
	}
	if bash.Parent != "" {
		t.Errorf("lead Bash call should have empty Parent, got %q", bash.Parent)
	}
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(bash.Input, &in); err != nil {
		t.Fatalf("decode Bash input: %v", err)
	}
	if in.Command != "echo hello-flanders" {
		t.Errorf("Bash command = %q, want %q", in.Command, "echo hello-flanders")
	}
	// tool_result reconciled back onto the call
	if bash.IsError {
		t.Errorf("Bash result should not be an error")
	}
	if !strings.Contains(bash.Result, "hello-flanders") {
		t.Errorf("Bash result = %q, want it to contain hello-flanders", bash.Result)
	}

	// --- text extraction ---
	joined := strings.Join(obs.Texts, "\n")
	if !strings.Contains(joined, "hello-flanders") {
		t.Errorf("assistant text %q should mention hello-flanders", joined)
	}

	// --- result event: outcome, cost, final usage, context window ---
	if !obs.Done {
		t.Error("Done should be true after a result event")
	}
	if obs.Subtype != "success" || obs.IsError {
		t.Errorf("Subtype=%q IsError=%v, want success/false", obs.Subtype, obs.IsError)
	}
	if !strings.Contains(obs.ResultText, "hello-flanders") {
		t.Errorf("ResultText = %q", obs.ResultText)
	}
	if math.Abs(obs.Cost-0.03455709999999999) > 1e-9 {
		t.Errorf("Cost = %v, want ~0.0345571", obs.Cost)
	}
	if got := obs.FinalUsage.Total(); got != 4+5988+32507+120 {
		t.Errorf("FinalUsage.Total() = %d, want 38619", got)
	}
	if obs.ContextWindow != 200000 {
		t.Errorf("ContextWindow = %d, want 200000 (reported by CLI modelUsage)", obs.ContextWindow)
	}

	// --- token usage / occupancy: peak is lead-only and within the window ---
	if obs.PeakLeadTokens <= 0 {
		t.Errorf("PeakLeadTokens = %d, want > 0 (from stream_event usage)", obs.PeakLeadTokens)
	}
	if occ := obs.Occupancy(0); occ <= 0 || occ >= 1 {
		t.Errorf("Occupancy = %v, want in (0,1)", occ)
	}

	// --- rate_limit_event: clean epoch reset, type, not limited ---
	if obs.RateLimitType != "five_hour" {
		t.Errorf("RateLimitType = %q, want five_hour", obs.RateLimitType)
	}
	if obs.ResetAt == nil {
		t.Fatal("ResetAt should be set from the rate_limit_event")
	}
	if obs.ResetAt.Unix() != 1779691800 {
		t.Errorf("ResetAt epoch = %d, want 1779691800", obs.ResetAt.Unix())
	}
	if obs.UsageLimited {
		t.Error("UsageLimited should be false (status was 'allowed')")
	}
}

// TestObserveSubagent pins subagent-spawn detection and parent attribution.
func TestObserveSubagent(t *testing.T) {
	obs, err := Observe(openFixture(t, "subagent.jsonl"), nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(obs.Subagents) < 1 {
		t.Fatalf("expected >=1 subagent spawn, got %d", len(obs.Subagents))
	}
	sp := obs.Subagents[0]
	if sp.SubagentType != "general-purpose" {
		t.Errorf("SubagentType = %q, want general-purpose", sp.SubagentType)
	}
	if sp.Description == "" {
		t.Error("subagent Description should be populated from Task input")
	}
	// The spawn corresponds to a Task/Agent tool_use (CLI 2.1.150 names it
	// "Agent"; older/other builds may use "Task" — the parser accepts both).
	var spawnCall *ToolCall
	for i := range obs.ToolCalls {
		if obs.ToolCalls[i].ID == sp.ToolUseID {
			spawnCall = &obs.ToolCalls[i]
		}
	}
	if spawnCall == nil {
		t.Fatalf("no tool_use matches the recorded spawn id %q", sp.ToolUseID)
	}
	if spawnCall.Name != "Agent" && spawnCall.Name != "Task" {
		t.Errorf("spawn tool name = %q, want Agent or Task", spawnCall.Name)
	}
	// The subagent's inner tool calls carry a non-empty Parent — proof attribution
	// works. (The subagent ran a Bash echo inside the Task.)
	var sawNested bool
	for _, tc := range obs.ToolCalls {
		if tc.Parent != "" {
			sawNested = true
		}
	}
	if !sawNested {
		t.Error("expected at least one nested (subagent) tool call with a Parent")
	}
	if !obs.Done || obs.IsError {
		t.Errorf("subagent run should finish cleanly: Done=%v IsError=%v", obs.Done, obs.IsError)
	}
}

// TestSkipUnparseableLine: a malformed line is skipped, surrounding lines parse,
// the loop never crashes (specs/08 §rules).
func TestSkipUnparseableLine(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"system","subtype":"init","model":"claude-opus-4-7","session_id":"s1"}`,
		`{this is not valid json at all`,
		`{"type":"result","subtype":"success","is_error":false,"total_cost_usd":1.5,"session_id":"s1"}`,
	}, "\n")
	obs, err := Observe(strings.NewReader(input), nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", obs.Skipped)
	}
	if obs.Lines != 2 {
		t.Errorf("Lines = %d, want 2 (system + result)", obs.Lines)
	}
	if obs.Model != "claude-opus-4-7" {
		t.Errorf("Model = %q, want the line before the bad one to parse", obs.Model)
	}
	if !obs.Done || obs.Cost != 1.5 {
		t.Errorf("the line after the bad one must parse: Done=%v Cost=%v", obs.Done, obs.Cost)
	}
}

// TestUnknownTypePreserved: an unknown top-level type is preserved verbatim and
// ignored by feature logic (forward-compatible — specs/08 §rules).
func TestUnknownTypePreserved(t *testing.T) {
	line := `{"type":"some_future_event","payload":{"a":1},"session_id":"s9"}`
	d := NewDecoder(strings.NewReader(line), nil)
	ev, err := d.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Type != "some_future_event" {
		t.Errorf("Type = %q", ev.Type)
	}
	if ev.SessionID != "s9" {
		t.Errorf("envelope SessionID should still decode, got %q", ev.SessionID)
	}
	if ev.System != nil || ev.Result != nil || ev.RateLimit != nil || ev.Assistant != nil {
		t.Error("unknown type must leave all typed payloads nil")
	}
	if len(ev.Raw) == 0 || !json.Valid(ev.Raw) {
		t.Error("Raw must preserve the verbatim line for the journal")
	}
}

// TestNoTrailingNewline: a final line without a trailing newline is still decoded.
func TestNoTrailingNewline(t *testing.T) {
	line := `{"type":"result","subtype":"success","total_cost_usd":2.0}` // no "\n"
	obs, err := Observe(strings.NewReader(line), nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !obs.Done || obs.Cost != 2.0 {
		t.Errorf("trailing line without newline not decoded: Done=%v Cost=%v", obs.Done, obs.Cost)
	}
}

// TestStreamChannel: the channel API yields the same events and is cancellable.
func TestStreamChannel(t *testing.T) {
	var sawResult bool
	var count int
	for ev := range Stream(context.Background(), openFixture(t, "basic.jsonl"), nil) {
		count++
		if ev.Type == TypeResult {
			sawResult = true
		}
	}
	if count == 0 {
		t.Fatal("Stream yielded no events")
	}
	if !sawResult {
		t.Error("Stream did not yield the result event")
	}
}

func TestRateLimitInfo(t *testing.T) {
	ri := RateLimitInfo{Status: "allowed", ResetsAt: 1779691800, RateLimitType: "five_hour"}
	if ri.Limited() {
		t.Error("status 'allowed' must not be Limited")
	}
	if ri.ResetTime().Unix() != 1779691800 {
		t.Errorf("ResetTime = %v", ri.ResetTime())
	}
	if !(RateLimitInfo{Status: "rejected"}).Limited() {
		t.Error("a non-allowed status must be Limited")
	}
	if (RateLimitInfo{Status: ""}).Limited() {
		t.Error("an empty status must not be Limited (no signal)")
	}
	if !(RateLimitInfo{ResetsAt: 0}).ResetTime().IsZero() {
		t.Error("zero ResetsAt must yield a zero time")
	}
}

func TestUsageTotalAndOccupancy(t *testing.T) {
	u := Usage{InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: 100, CacheCreationInputTokens: 20}
	if u.Total() != 135 {
		t.Errorf("Total() = %d, want 135", u.Total())
	}
	obs := &LoopObservation{PeakLeadTokens: 50000}
	if got := obs.Occupancy(200000); math.Abs(got-0.25) > 1e-9 {
		t.Errorf("Occupancy(200000) = %v, want 0.25", got)
	}
	obs.ContextWindow = 100000
	if got := obs.Occupancy(0); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("Occupancy(0) should fall back to ContextWindow: = %v, want 0.5", got)
	}
	if got := (&LoopObservation{PeakLeadTokens: 1}).Occupancy(0); got != 0 {
		t.Errorf("Occupancy with no known window = %v, want 0", got)
	}
}
