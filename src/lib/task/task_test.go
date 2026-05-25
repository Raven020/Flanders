package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// canonical is the example task file from specs/02-plan-and-tasks.md, verbatim.
// It exercises every round-trip hazard at once: a zero-padded id, an inline
// comment, a flow-style numeric list, a double-quoted string, and a markdown body
// containing backticks and a non-ASCII section sign.
const canonical = "---\n" +
	"id: 0007\n" +
	"status: pending          # pending | active | done | blocked\n" +
	"deps: [0001, 0005]       # task ids that must be `done` first\n" +
	"acceptance: \"go test ./engine passes; stall detection halts after N no-change loops\"\n" +
	"---\n" +
	"## Loop engine\n" +
	"\n" +
	"Drive fresh `claude -p` invocations, parse stream-json, checkpoint per loop.\n" +
	"References: specs/01-ralph-loop.md §Iteration anatomy.\n"

const canonicalBody = "## Loop engine\n" +
	"\n" +
	"Drive fresh `claude -p` invocations, parse stream-json, checkpoint per loop.\n" +
	"References: specs/01-ralph-loop.md §Iteration anatomy.\n"

func TestParseAccessors(t *testing.T) {
	tk, err := Parse([]byte(canonical))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := tk.ID(); got != "0007" { // leading zeros must survive
		t.Errorf("ID() = %q, want 0007", got)
	}
	if got := tk.Status(); got != StatusPending {
		t.Errorf("Status() = %q, want pending", got)
	}
	if got := tk.Reason(); got != "" {
		t.Errorf("Reason() = %q, want empty", got)
	}
	if got := tk.Deps(); len(got) != 2 || got[0] != "0001" || got[1] != "0005" {
		t.Errorf("Deps() = %v, want [0001 0005]", got)
	}
	if got := tk.Acceptance(); got != "go test ./engine passes; stall detection halts after N no-change loops" {
		t.Errorf("Acceptance() = %q", got)
	}
	if got := tk.Body; got != canonicalBody {
		t.Errorf("Body mismatch:\n got %q\nwant %q", got, canonicalBody)
	}
	if err := tk.Validate(); err != nil {
		t.Errorf("Validate() on canonical = %v, want nil", err)
	}
}

func TestRoundTripLossless(t *testing.T) {
	tk, err := Parse([]byte(canonical))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := tk.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	// The inline comment is a lossless-round-trip hazard a struct decode would
	// drop; the node decode must keep it.
	if !strings.Contains(string(out), "# pending | active | done | blocked") {
		t.Errorf("round-trip dropped the inline comment:\n%s", out)
	}
	// Re-parse the serialized form and confirm fields + body are unchanged.
	again, err := Parse(out)
	if err != nil {
		t.Fatalf("re-Parse: %v", err)
	}
	if again.ID() != tk.ID() || again.Status() != tk.Status() ||
		again.Acceptance() != tk.Acceptance() || again.Body != tk.Body {
		t.Errorf("re-parsed fields drifted:\n first=%+v\nsecond=%+v", tk, again)
	}
	if d := again.Deps(); len(d) != 2 || d[0] != "0001" || d[1] != "0005" {
		t.Errorf("re-parsed Deps = %v", d)
	}
}

func TestRoundTripPreservesUnknownFields(t *testing.T) {
	// `priority` is not in the schema. The spec leaves room for future fields and
	// requires lossless round-trip, so an unknown key must survive untouched.
	src := "---\n" +
		"id: 12\n" +
		"status: active\n" +
		"deps: []\n" +
		"acceptance: \"x\"\n" +
		"priority: high\n" +
		"owner: raven\n" +
		"---\nbody text\n"
	tk, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Mutate a known field; unknown fields must still be there afterward.
	tk.SetStatus(StatusDone)
	out, err := tk.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "priority: high") {
		t.Errorf("dropped unknown field 'priority':\n%s", s)
	}
	if !strings.Contains(s, "owner: raven") {
		t.Errorf("dropped unknown field 'owner':\n%s", s)
	}
	if !strings.Contains(s, "status: done") {
		t.Errorf("status not updated:\n%s", s)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantErr bool
	}{
		{"valid pending", "---\nid: 1\nstatus: pending\nacceptance: \"a\"\n---\n", false},
		{"valid blocked + reason", "---\nid: 1\nstatus: blocked\nreason: context-overreach\nacceptance: \"a\"\n---\n", false},
		{"blocked without reason", "---\nid: 1\nstatus: blocked\nacceptance: \"a\"\n---\n", true},
		{"blocked invalid reason", "---\nid: 1\nstatus: blocked\nreason: bored\nacceptance: \"a\"\n---\n", true},
		{"reason without blocked", "---\nid: 1\nstatus: active\nreason: dependency\nacceptance: \"a\"\n---\n", true},
		{"invalid status", "---\nid: 1\nstatus: wibble\nacceptance: \"a\"\n---\n", true},
		{"missing id", "---\nstatus: pending\nacceptance: \"a\"\n---\n", true},
		{"missing acceptance", "---\nid: 1\nstatus: pending\n---\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tk, err := Parse([]byte(tc.src))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			err = tk.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestSetStatusClearsReason(t *testing.T) {
	// Moving off 'blocked' must drop the now-meaningless reason so the
	// "reason iff blocked" invariant holds without the caller managing it.
	tk, err := Parse([]byte("---\nid: 1\nstatus: blocked\nreason: dependency\nacceptance: \"a\"\n---\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tk.SetStatus(StatusDone)
	if r := tk.Reason(); r != "" {
		t.Errorf("Reason() = %q after SetStatus(done), want empty", r)
	}
	if err := tk.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestSetBlocked(t *testing.T) {
	tk := New("9", StatusActive, nil, "acc", "body")
	tk.SetBlocked(ReasonContextOverreach)
	if tk.Status() != StatusBlocked {
		t.Errorf("Status() = %q, want blocked", tk.Status())
	}
	if tk.Reason() != ReasonContextOverreach {
		t.Errorf("Reason() = %q, want context-overreach", tk.Reason())
	}
	if err := tk.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestNewRoundTrips(t *testing.T) {
	tk := New("0007", StatusPending, []string{"0001", "0005"}, "go test ./... passes", "## Title\n\nbody\n")
	if err := tk.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	out, err := tk.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	again, err := Parse(out)
	if err != nil {
		t.Fatalf("re-Parse: %v\n%s", err, out)
	}
	if again.ID() != "0007" || again.Status() != StatusPending {
		t.Errorf("re-parsed id/status = %q/%q", again.ID(), again.Status())
	}
	if d := again.Deps(); len(d) != 2 || d[0] != "0001" || d[1] != "0005" {
		t.Errorf("re-parsed Deps = %v", d)
	}
	if again.Body != "## Title\n\nbody\n" {
		t.Errorf("re-parsed Body = %q", again.Body)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"no frontmatter": "id: 1\nstatus: pending\n",
		"unterminated":   "---\nid: 1\nstatus: pending\n",
		"empty":          "",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(src)); err == nil {
				t.Errorf("Parse(%q) = nil error, want error", src)
			}
		})
	}
}

func TestBodyWithHorizontalRule(t *testing.T) {
	// A `---` line inside the body is a markdown horizontal rule, not the
	// frontmatter fence. Only the first `---` after the opener closes the block.
	src := "---\nid: 1\nstatus: pending\nacceptance: \"a\"\n---\nintro\n\n---\n\nmore\n"
	tk, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wantBody := "intro\n\n---\n\nmore\n"
	if tk.Body != wantBody {
		t.Errorf("Body = %q, want %q", tk.Body, wantBody)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "0007-loop-engine.md")
	tk, err := Parse([]byte(canonical))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := tk.WriteFile(path); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if tk.Path != path {
		t.Errorf("Path = %q after WriteFile, want %q", tk.Path, path)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want, _ := tk.Bytes()
	if string(onDisk) != string(want) {
		t.Errorf("on-disk content != Bytes():\n got %q\nwant %q", onDisk, want)
	}
	// No temp files should be left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("dir has %d entries, want 1 (a leftover temp file?)", len(entries))
	}
	// And the file we wrote must re-parse cleanly.
	if _, err := ParseFile(path); err != nil {
		t.Errorf("ParseFile after WriteFile: %v", err)
	}
}
