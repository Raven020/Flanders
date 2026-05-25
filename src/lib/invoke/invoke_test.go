package invoke

import (
	"reflect"
	"regexp"
	"testing"

	"flanders/src/lib/config"
)

// baseCfg returns a fresh default config for tests to tweak. Default() already
// carries every documented spec-03 default, so tests only override the one field
// they exercise.
func baseCfg() *config.Config {
	c := config.Default()
	return &c
}

// TestBuildDefaultPlan locks the full argv for the common case: plan phase,
// bypass permissions, stream_input on, rules appended. This is the spec-01/03
// acceptance ("builder emits expected argv per phase/config") as an exact-match
// assertion — the strongest contract, so any accidental reordering or dropped
// flag fails loudly.
func TestBuildDefaultPlan(t *testing.T) {
	cmd, err := Build(baseCfg(), Spec{
		Phase:              "plan",
		SessionID:          "SID",
		SystemPromptAppend: "RULES",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := []string{
		"claude",
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--session-id", "SID",
		"--dangerously-skip-permissions",
		"--model", "opus",
		"--effort", "high",
		"--append-system-prompt", "RULES",
		"--input-format", "stream-json",
	}
	if got := cmd.Argv(); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv mismatch:\n got: %v\nwant: %v", got, want)
	}
}

// TestBuildStreamInputOffPositionalPrompt: with stream_input disabled there is no
// stdin channel, so the prompt rides as the trailing positional to `claude -p`
// and --input-format is omitted.
func TestBuildStreamInputOffPositionalPrompt(t *testing.T) {
	cfg := baseCfg()
	cfg.Agent.StreamInput = false
	cmd, err := Build(cfg, Spec{
		Phase:     "build",
		SessionID: "SID",
		Prompt:    "implement task 1",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := []string{
		"claude",
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--session-id", "SID",
		"--dangerously-skip-permissions",
		"--model", "opus",
		"--effort", "high",
		"implement task 1",
	}
	if got := cmd.Argv(); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv mismatch:\n got: %v\nwant: %v", got, want)
	}
}

// TestBuildStreamInputOnIgnoresPositionalPrompt: when stream_input is on the
// prompt is delivered over stdin, so even a set Prompt must NOT appear in argv —
// it would otherwise be a duplicate/contradictory turn.
func TestBuildStreamInputOnIgnoresPositionalPrompt(t *testing.T) {
	cmd, err := Build(baseCfg(), Spec{
		Phase:     "build",
		SessionID: "SID",
		Prompt:    "should not appear",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, a := range cmd.Args {
		if a == "should not appear" {
			t.Fatalf("prompt leaked into argv under stream_input: %v", cmd.Argv())
		}
	}
	if !hasFlag(cmd.Args, "--input-format") {
		t.Fatalf("expected --input-format under stream_input: %v", cmd.Argv())
	}
}

// TestNeverEmitsBudget guards the subscription invariant: --max-budget-usd must
// never be present (spec 00/01).
func TestNeverEmitsBudget(t *testing.T) {
	cmd, _ := Build(baseCfg(), Spec{Phase: "plan", SessionID: "SID"})
	if hasFlag(cmd.Args, "--max-budget-usd") {
		t.Fatalf("--max-budget-usd must never be emitted: %v", cmd.Argv())
	}
}

// TestSystemPromptOmittedWhenEmpty: no rules text → no --append-system-prompt.
func TestSystemPromptOmittedWhenEmpty(t *testing.T) {
	cmd, _ := Build(baseCfg(), Spec{Phase: "plan", SessionID: "SID"})
	if hasFlag(cmd.Args, "--append-system-prompt") {
		t.Fatalf("--append-system-prompt should be omitted when empty: %v", cmd.Argv())
	}
}

// TestPermissionModes maps every config permission_mode to its expected flag(s).
func TestPermissionModes(t *testing.T) {
	cases := []struct {
		mode string
		want []string // the flag fragment expected to be present
		// absent: --dangerously-skip-permissions present iff mode==bypass
	}{
		{"bypassPermissions", []string{"--dangerously-skip-permissions"}},
		{"acceptEdits", []string{"--permission-mode", "acceptEdits"}},
		{"default", []string{"--permission-mode", "default"}},
		{"plan", []string{"--permission-mode", "plan"}},
	}
	for _, tc := range cases {
		cfg := baseCfg()
		cfg.Agent.PermissionMode = tc.mode
		cmd, err := Build(cfg, Spec{Phase: "plan", SessionID: "SID"})
		if err != nil {
			t.Fatalf("%s: Build: %v", tc.mode, err)
		}
		if !containsSeq(cmd.Args, tc.want) {
			t.Fatalf("%s: expected %v in %v", tc.mode, tc.want, cmd.Argv())
		}
		// bypass must NOT also carry --permission-mode, and the others must NOT
		// carry the bypass shortcut.
		if tc.mode == "bypassPermissions" && hasFlag(cmd.Args, "--permission-mode") {
			t.Fatalf("bypass should not also emit --permission-mode: %v", cmd.Argv())
		}
		if tc.mode != "bypassPermissions" && hasFlag(cmd.Args, "--dangerously-skip-permissions") {
			t.Fatalf("%s should not emit --dangerously-skip-permissions: %v", tc.mode, cmd.Argv())
		}
	}
}

// TestPerPhaseModelEffort: each phase resolves to its configured class, and split
// reuses plan (spec 07). Asserts on the resolved --model/--effort pair.
func TestPerPhaseModelEffort(t *testing.T) {
	cases := []struct {
		phase, model, effort string
	}{
		{"discuss", "opus", "high"},
		{"plan", "opus", "high"},
		{"build", "opus", "high"},
		{"test", "sonnet", "medium"},
		{"split", "opus", "high"}, // reuses plan
	}
	for _, tc := range cases {
		cmd, err := Build(baseCfg(), Spec{Phase: tc.phase, SessionID: "SID"})
		if err != nil {
			t.Fatalf("%s: Build: %v", tc.phase, err)
		}
		if !containsSeq(cmd.Args, []string{"--model", tc.model}) {
			t.Fatalf("%s: want --model %s in %v", tc.phase, tc.model, cmd.Argv())
		}
		if !containsSeq(cmd.Args, []string{"--effort", tc.effort}) {
			t.Fatalf("%s: want --effort %s in %v", tc.phase, tc.effort, cmd.Argv())
		}
	}
}

// TestBinOverride: the binary comes from [agent].bin.
func TestBinOverride(t *testing.T) {
	cfg := baseCfg()
	cfg.Agent.Bin = "/opt/claude/bin/claude"
	cmd, _ := Build(cfg, Spec{Phase: "plan", SessionID: "SID"})
	if cmd.Bin != "/opt/claude/bin/claude" {
		t.Fatalf("bin = %q, want the configured path", cmd.Bin)
	}
	if cmd.Argv()[0] != "/opt/claude/bin/claude" {
		t.Fatalf("argv[0] = %q, want the configured bin", cmd.Argv()[0])
	}
}

func TestBuildErrors(t *testing.T) {
	if _, err := Build(baseCfg(), Spec{Phase: "plan"}); err == nil {
		t.Fatal("expected error on empty SessionID")
	}
	if _, err := Build(baseCfg(), Spec{Phase: "nope", SessionID: "SID"}); err == nil {
		t.Fatal("expected error on unknown phase")
	}
	if _, err := Build(nil, Spec{Phase: "plan", SessionID: "SID"}); err == nil {
		t.Fatal("expected error on nil config")
	}
	cfg := baseCfg()
	cfg.Agent.PermissionMode = "bogus"
	if _, err := Build(cfg, Spec{Phase: "plan", SessionID: "SID"}); err == nil {
		t.Fatal("expected error on unknown permission_mode")
	}
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestNewSessionID: well-formed v4 UUID and unique across calls (each loop must
// get a distinct fresh session — spec 01).
func TestNewSessionID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := NewSessionID()
		if err != nil {
			t.Fatalf("NewSessionID: %v", err)
		}
		if !uuidV4.MatchString(id) {
			t.Fatalf("not a v4 uuid: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate session id: %q", id)
		}
		seen[id] = true
	}
}

// hasFlag reports whether flag appears anywhere in args.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// containsSeq reports whether seq appears as a contiguous run inside args.
func containsSeq(args, seq []string) bool {
	if len(seq) == 0 {
		return true
	}
	for i := 0; i+len(seq) <= len(args); i++ {
		if reflect.DeepEqual(args[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}
