package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeConfig writes content to a config.toml inside a fresh temp dir and returns
// its path. Tests stay self-contained — no shared fixtures on disk.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// fullSample mirrors the proposed file in specs/03-config.md, with a test command
// set and one per-class subagent override added, so a load exercises every section.
const fullSample = `
[paths]
specs   = "myspecs"
tasks   = "myspecs/tasks"
journal = ".flanders/journal"
plan    = "PLAN.md"
state   = ".flanders/state.json"

[commands]
test  = "go test ./..."
build = "go build ./..."
lint  = "golangci-lint run"

[agent]
bin             = "claude"
permission_mode = "bypassPermissions"
rules_file      = ".flanders/rules.md"
stream_input    = true

[phases.discuss]
model  = "opus"
effort = "high"

[phases.plan]
model  = "opus"
effort = "high"

[phases.build]
model  = "opus"
effort = "high"

[phases.test]
model  = "sonnet"
effort = "medium"

[subagents]
model  = "sonnet"
effort = "low"

[subagents.explore]
model  = "haiku"
effort = "low"

[context]
window_tokens = 200000
soft_pct      = 0.75
hard_pct      = 0.90

[guardrails]
max_iterations    = 100
stall_n           = 3
iteration_timeout = "20m"

[usage]
on_limit   = "wait"
max_cycles = 0
backoff    = "30m"

[git]
enabled         = true
init_if_missing = true
commit_each     = "progress"
message_tmpl    = "Flanders: {phase} #{iter} — {task} [{result}]"
`

// TestDefault must return every documented default from spec 03, and must leave
// commands.test empty (it has no default; the build phase requires it).
func TestDefault(t *testing.T) {
	d := Default()
	if d.Commands.Test != "" {
		t.Errorf("commands.test default = %q, want empty (no default by design)", d.Commands.Test)
	}
	if d.Agent.PermissionMode != "bypassPermissions" {
		t.Errorf("permission_mode default = %q, want bypassPermissions", d.Agent.PermissionMode)
	}
	if !d.Agent.StreamInput {
		t.Error("stream_input default = false, want true")
	}
	if d.Context.WindowTokens != 200000 {
		t.Errorf("window_tokens default = %d, want 200000", d.Context.WindowTokens)
	}
	if d.Context.SoftPct != 0.75 || d.Context.HardPct != 0.90 {
		t.Errorf("pct defaults = soft %v hard %v, want 0.75 / 0.90", d.Context.SoftPct, d.Context.HardPct)
	}
	if d.Guardrails.MaxIterations != 100 || d.Guardrails.StallN != 3 {
		t.Errorf("guardrail defaults = max %d stall %d, want 100 / 3", d.Guardrails.MaxIterations, d.Guardrails.StallN)
	}
	if d.Guardrails.IterationTimeout.Duration != 20*time.Minute {
		t.Errorf("iteration_timeout default = %v, want 20m", d.Guardrails.IterationTimeout.Duration)
	}
	if d.Usage.OnLimit != "wait" || d.Usage.Backoff.Duration != 30*time.Minute {
		t.Errorf("usage defaults = on_limit %q backoff %v, want wait / 30m", d.Usage.OnLimit, d.Usage.Backoff.Duration)
	}
	if d.Phases.Build.Model != "opus" || d.Phases.Build.Effort != "high" {
		t.Errorf("phases.build default = %v, want opus/high", d.Phases.Build)
	}
	if d.Phases.Test.Model != "sonnet" || d.Phases.Test.Effort != "medium" {
		t.Errorf("phases.test default = %v, want sonnet/medium", d.Phases.Test)
	}
	if d.Subagents.Model != "sonnet" || d.Subagents.Effort != "low" {
		t.Errorf("subagents default = %v, want sonnet/low", d.Subagents)
	}
	// Default() must validate clean — defaults are a valid config (except the
	// build-only test requirement).
	if err := d.Validate(); err != nil {
		t.Errorf("Default() failed Validate: %v", err)
	}
}

// TestLoadFullSample must parse every section of the proposed config, including
// duration strings and a per-class subagent override.
func TestLoadFullSample(t *testing.T) {
	cfg, err := Load(writeConfig(t, fullSample))
	if err != nil {
		t.Fatalf("Load full sample: %v", err)
	}
	if cfg.Paths.Specs != "myspecs" || cfg.Paths.Plan != "PLAN.md" {
		t.Errorf("paths = %+v, want overridden specs/plan", cfg.Paths)
	}
	if cfg.Commands.Test != "go test ./..." || cfg.Commands.Lint != "golangci-lint run" {
		t.Errorf("commands = %+v", cfg.Commands)
	}
	if cfg.Guardrails.IterationTimeout.Duration != 20*time.Minute {
		t.Errorf("iteration_timeout = %v, want 20m", cfg.Guardrails.IterationTimeout.Duration)
	}
	if cfg.Usage.Backoff.Duration != 30*time.Minute {
		t.Errorf("backoff = %v, want 30m", cfg.Usage.Backoff.Duration)
	}
	if cfg.Phases.Discuss.Model != "opus" || cfg.Phases.Test.Effort != "medium" {
		t.Errorf("phases = %+v", cfg.Phases)
	}
	explore, ok := cfg.Subagents.Classes["explore"]
	if !ok {
		t.Fatalf("subagents.explore override missing; classes = %+v", cfg.Subagents.Classes)
	}
	if explore.Model != "haiku" || explore.Effort != "low" {
		t.Errorf("subagents.explore = %+v, want haiku/low", explore)
	}
	if err := cfg.ValidateForBuild(); err != nil {
		t.Errorf("full sample failed ValidateForBuild: %v", err)
	}
}

// TestLoadMinimalAppliesDefaults must fill every omitted key from Default() while
// honoring the few keys the minimal file does set.
func TestLoadMinimalAppliesDefaults(t *testing.T) {
	minimal := `
[commands]
test = "make check"
`
	cfg, err := Load(writeConfig(t, minimal))
	if err != nil {
		t.Fatalf("Load minimal: %v", err)
	}
	if cfg.Commands.Test != "make check" {
		t.Errorf("commands.test = %q, want make check", cfg.Commands.Test)
	}
	// Everything else must equal the documented defaults.
	if cfg.Paths.Specs != "specs" || cfg.Paths.Tasks != "specs/tasks" {
		t.Errorf("paths not defaulted: %+v", cfg.Paths)
	}
	if cfg.Agent.Bin != "claude" || cfg.Agent.PermissionMode != "bypassPermissions" {
		t.Errorf("agent not defaulted: %+v", cfg.Agent)
	}
	if cfg.Context.SoftPct != 0.75 || cfg.Guardrails.MaxIterations != 100 {
		t.Errorf("thresholds not defaulted: ctx=%+v guard=%+v", cfg.Context, cfg.Guardrails)
	}
	if cfg.Guardrails.IterationTimeout.Duration != 20*time.Minute {
		t.Errorf("iteration_timeout not defaulted: %v", cfg.Guardrails.IterationTimeout.Duration)
	}
	if cfg.Git.CommitEach != "progress" {
		t.Errorf("git not defaulted: %+v", cfg.Git)
	}
}

// TestValidateForBuild must reject a config with no test command but accept one
// that sets it — the test command is the build phase's ground-truth gate.
func TestValidateForBuild(t *testing.T) {
	noTest, err := Load(writeConfig(t, "[agent]\nbin = \"claude\"\n"))
	if err != nil {
		t.Fatalf("Load (no test): %v", err)
	}
	if err := noTest.ValidateForBuild(); err == nil {
		t.Error("ValidateForBuild accepted a config with no test command")
	}

	withTest, err := Load(writeConfig(t, "[commands]\ntest = \"go test ./...\"\n"))
	if err != nil {
		t.Fatalf("Load (with test): %v", err)
	}
	if err := withTest.ValidateForBuild(); err != nil {
		t.Errorf("ValidateForBuild rejected a valid test command: %v", err)
	}

	// A whitespace-only test command counts as missing.
	blank, err := Load(writeConfig(t, "[commands]\ntest = \"   \"\n"))
	if err != nil {
		t.Fatalf("Load (blank test): %v", err)
	}
	if err := blank.ValidateForBuild(); err == nil {
		t.Error("ValidateForBuild accepted a whitespace-only test command")
	}
}

// TestValidateRejectsBadEnums must reject out-of-set enum values.
func TestValidateRejectsBadEnums(t *testing.T) {
	cases := map[string]string{
		"bad effort":          "[phases.build]\neffort = \"turbo\"\n",
		"bad on_limit":        "[usage]\non_limit = \"maybe\"\n",
		"bad commit_each":     "[git]\ncommit_each = \"sometimes\"\n",
		"bad permission_mode": "[agent]\npermission_mode = \"yolo\"\n",
		"bad subagent effort": "[subagents.explore]\neffort = \"ultra\"\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, content)); err == nil {
				t.Errorf("Load accepted invalid config (%s)", name)
			}
		})
	}
}

// TestValidateRejectsBadThresholds must reject out-of-range numeric thresholds.
func TestValidateRejectsBadThresholds(t *testing.T) {
	cases := map[string]string{
		"soft > hard":     "[context]\nsoft_pct = 0.95\nhard_pct = 0.80\n",
		"hard > 1":        "[context]\nhard_pct = 1.5\n",
		"soft <= 0":       "[context]\nsoft_pct = 0\n",
		"max_iter < 1":    "[guardrails]\nmax_iterations = 0\n",
		"stall_n < 1":     "[guardrails]\nstall_n = 0\n",
		"timeout <= 0":    "[guardrails]\niteration_timeout = \"0s\"\n",
		"backoff <= 0":    "[usage]\nbackoff = \"0s\"\n",
		"window negative": "[context]\nwindow_tokens = -1\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, content)); err == nil {
				t.Errorf("Load accepted invalid config (%s)", name)
			}
		})
	}
}

// TestLoadRejectsBadDuration must surface an unparseable duration string rather
// than silently zeroing it.
func TestLoadRejectsBadDuration(t *testing.T) {
	if _, err := Load(writeConfig(t, "[guardrails]\niteration_timeout = \"20 minutes\"\n")); err == nil {
		t.Error("Load accepted an unparseable duration")
	}
}

// TestLoadMissingFile must return an error that unwraps to fs.ErrNotExist, so the
// caller can detect "no config yet" and run `flanders init`.
func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err == nil {
		t.Fatal("Load of missing file returned no error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load error does not unwrap to fs.ErrNotExist: %v", err)
	}
}

// TestWindowTokensZeroAllowed must accept window_tokens = 0 (means auto-detect by
// model), distinct from a negative value which is rejected.
func TestWindowTokensZeroAllowed(t *testing.T) {
	cfg, err := Load(writeConfig(t, "[context]\nwindow_tokens = 0\n"))
	if err != nil {
		t.Fatalf("Load window_tokens=0: %v", err)
	}
	if cfg.Context.WindowTokens != 0 {
		t.Errorf("window_tokens = %d, want 0", cfg.Context.WindowTokens)
	}
}

// TestPhaseClass covers phase→class resolution including split→plan reuse and the
// unknown-phase error (spec 07). This is the single source of truth the invoke
// builder relies on, so a wrong mapping would silently run the wrong model.
func TestPhaseClass(t *testing.T) {
	c := Default()
	cases := []struct {
		phase, model, effort string
	}{
		{"discuss", "opus", "high"},
		{"plan", "opus", "high"},
		{"build", "opus", "high"},
		{"test", "sonnet", "medium"},
		{"split", "opus", "high"}, // split reuses plan
	}
	for _, tc := range cases {
		got, err := c.PhaseClass(tc.phase)
		if err != nil {
			t.Fatalf("%s: %v", tc.phase, err)
		}
		if got.Model != tc.model || got.Effort != tc.effort {
			t.Errorf("%s: got %s/%s, want %s/%s", tc.phase, got.Model, got.Effort, tc.model, tc.effort)
		}
	}
	if _, err := c.PhaseClass("bogus"); err == nil {
		t.Error("expected error for unknown phase")
	}
}

func TestSubagentClass(t *testing.T) {
	// Default config has no per-class overrides: every name resolves to the
	// global [subagents] default, and unknown names never error.
	d := Default()
	for _, name := range []string{"explore", "review", "anything"} {
		got := d.SubagentClass(name)
		if got.Model != "sonnet" || got.Effort != "low" {
			t.Errorf("default SubagentClass(%q) = %s/%s, want sonnet/low", name, got.Model, got.Effort)
		}
	}

	// A loaded config with overrides merges them onto the default.
	cfg, err := Load(writeConfig(t, fullSample))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// fullSample defines [subagents.explore] model=haiku, effort=low (full override).
	if got := cfg.SubagentClass("explore"); got.Model != "haiku" || got.Effort != "low" {
		t.Errorf("SubagentClass(explore) = %s/%s, want haiku/low", got.Model, got.Effort)
	}
	// An unconfigured name still falls back to the global default.
	if got := cfg.SubagentClass("unconfigured"); got.Model != "sonnet" || got.Effort != "low" {
		t.Errorf("SubagentClass(unconfigured) = %s/%s, want sonnet/low", got.Model, got.Effort)
	}
}

// TestSubagentClassPartialOverride locks the field-by-field merge: an override
// that sets only one field must keep the global default for the other, never
// blank it to "".
func TestSubagentClassPartialOverride(t *testing.T) {
	const partial = `
[commands]
test = "go test ./..."

[subagents]
model  = "sonnet"
effort = "low"

[subagents.modelonly]
model = "opus"

[subagents.effortonly]
effort = "high"
`
	cfg, err := Load(writeConfig(t, partial))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.SubagentClass("modelonly"); got.Model != "opus" || got.Effort != "low" {
		t.Errorf("SubagentClass(modelonly) = %s/%s, want opus/low (effort kept from default)", got.Model, got.Effort)
	}
	if got := cfg.SubagentClass("effortonly"); got.Model != "sonnet" || got.Effort != "high" {
		t.Errorf("SubagentClass(effortonly) = %s/%s, want sonnet/high (model kept from default)", got.Model, got.Effort)
	}
}
