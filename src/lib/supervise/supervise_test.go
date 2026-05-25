package supervise

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flanders/src/lib/invoke"
	"flanders/src/lib/stream"
)

// A minimal but real stream-json transcript: system/init, one assistant text
// block, and a success result. cat-ing this file is our deterministic stand-in for
// a `claude` invocation (the acceptance test uses a STUB command, per plan 2.5).
const fixtureJSONL = `{"type":"system","subtype":"init","session_id":"s1","model":"opus","permissionMode":"bypassPermissions","tools":[]}
{"type":"assistant","session_id":"s1","message":{"role":"assistant","model":"opus","content":[{"type":"text","text":"hello world"}]}}
{"type":"result","subtype":"success","is_error":false,"session_id":"s1","total_cost_usd":0.0123,"duration_ms":4321,"num_turns":1,"result":"done","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}
`

func writeFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(fixtureJSONL), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func catCmd(path string) invoke.Command  { return invoke.Command{Bin: "cat", Args: []string{path}} }
func shCmd(script string) invoke.Command { return invoke.Command{Bin: "sh", Args: []string{"-c", script}} }

func TestRunStreamsAndFolds(t *testing.T) {
	res, err := Run(context.Background(), Spec{Command: catCmd(writeFixture(t))})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.TimedOut || res.Canceled {
		t.Errorf("clean run flagged TimedOut=%v Canceled=%v", res.TimedOut, res.Canceled)
	}
	o := res.Observation
	if !o.Done || o.Subtype != "success" || o.IsError {
		t.Errorf("observation outcome wrong: Done=%v Subtype=%q IsError=%v", o.Done, o.Subtype, o.IsError)
	}
	if len(o.Texts) != 1 || o.Texts[0] != "hello world" {
		t.Errorf("Texts = %v, want [hello world]", o.Texts)
	}
	if o.SessionID != "s1" {
		t.Errorf("SessionID = %q, want s1", o.SessionID)
	}
	if o.Cost != 0.0123 {
		t.Errorf("Cost = %v, want 0.0123", o.Cost)
	}
	// Classify ties the observation to the exit code — the loop driver's decision.
	if got := o.Classify(res.ExitCode); got != stream.OutcomeSuccess {
		t.Errorf("Classify = %v, want OutcomeSuccess", got)
	}
}

func TestRunArchivesRawSink(t *testing.T) {
	var raw bytes.Buffer
	if _, err := Run(context.Background(), Spec{Command: catCmd(writeFixture(t)), RawSink: &raw}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if raw.String() != fixtureJSONL {
		t.Errorf("RawSink did not capture the verbatim transcript:\n got %q\nwant %q", raw.String(), fixtureJSONL)
	}
}

func TestOnEventHookSeesEvents(t *testing.T) {
	var types []string
	_, err := Run(context.Background(), Spec{
		Command: catCmd(writeFixture(t)),
		OnEvent: func(_ *Proc, ev *stream.Event) { types = append(types, ev.Type) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{stream.TypeSystem, stream.TypeAssistant, stream.TypeResult}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Errorf("hook saw %v, want %v", types, want)
	}
}

func TestRunCapturesStderr(t *testing.T) {
	res, err := Run(context.Background(), Spec{Command: shCmd("echo oops 1>&2; exit 0")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Stderr, "oops") {
		t.Errorf("Stderr = %q, want it to contain oops", res.Stderr)
	}
}

func TestRunReportsExitCode(t *testing.T) {
	res, err := Run(context.Background(), Spec{Command: shCmd("exit 7")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
	if res.Observation.Done {
		t.Errorf("a bare non-zero exit (no result event) should not be Done")
	}
	// No result event + non-zero exit ⇒ the classifier's error path.
	if got := res.Observation.Classify(res.ExitCode); got != stream.OutcomeError {
		t.Errorf("Classify = %v, want OutcomeError", got)
	}
}

func TestTimeoutKills(t *testing.T) {
	start := time.Now()
	res, err := Run(context.Background(), Spec{
		Command: shCmd("sleep 30"),
		Timeout: 150 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout did not kill promptly: took %v", elapsed)
	}
	if !res.TimedOut {
		t.Errorf("TimedOut = false, want true")
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 (killed by signal)", res.ExitCode)
	}
}

func TestContextCancelKills(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, err := Start(ctx, Spec{Command: shCmd("sleep 30")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res := p.Wait()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancel did not kill promptly: took %v", elapsed)
	}
	if !res.Canceled {
		t.Errorf("Canceled = false, want true")
	}
	if res.TimedOut {
		t.Errorf("TimedOut = true on an external cancel, want false")
	}
}

func TestStreamInputRequiresPrompt(t *testing.T) {
	_, err := Start(context.Background(), Spec{Command: catCmd(writeFixture(t)), StreamInput: true})
	if err == nil {
		t.Fatal("Start with StreamInput and no Prompt should error (un-missable stdin contract)")
	}
	if !strings.Contains(err.Error(), "Prompt") {
		t.Errorf("error = %v, want it to mention the missing Prompt", err)
	}
}

// The stdin channel: the initial prompt AND a later Inject must both reach the
// child as well-formed stream-json user lines. `cat` echoes stdin→stdout, so the
// RawSink (a tee of stdout) captures exactly what we wrote.
func TestStreamInputInjection(t *testing.T) {
	var raw bytes.Buffer
	p, err := Start(context.Background(), Spec{
		Command:     invoke.Command{Bin: "cat"}, // echoes stdin to stdout
		StreamInput: true,
		Prompt:      "do the task",
		RawSink:     &raw,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Inject("wrap up now"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := p.CloseInput(); err != nil { // let cat see EOF and exit
		t.Fatalf("CloseInput: %v", err)
	}
	res := p.Wait()
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(raw.String(), "do the task") {
		t.Errorf("initial prompt not written to stdin; stdout echo = %q", raw.String())
	}
	if !strings.Contains(raw.String(), "wrap up now") {
		t.Errorf("injected message not written to stdin; stdout echo = %q", raw.String())
	}
	// And what we wrote is decodable as two user turns (the round-trip the encoder promises).
	if res.Observation.Lines != 2 {
		t.Errorf("decoded %d lines, want 2 (prompt + injection)", res.Observation.Lines)
	}
}

func TestInjectWithoutStreamInputErrors(t *testing.T) {
	p, err := Start(context.Background(), Spec{Command: shCmd("sleep 0.1")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Inject("nope"); err == nil {
		t.Error("Inject without StreamInput should error")
	}
	p.Wait()
}

func TestEmptyCommandRejected(t *testing.T) {
	if _, err := Start(context.Background(), Spec{}); err == nil {
		t.Fatal("Start with an empty command should error")
	}
}
