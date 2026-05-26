package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flanders/src/lib/config"
	"flanders/src/lib/rules"
)

// TestDispatchUnknownCommand must reject an unknown command word with a usage
// hint rather than falling through to the orchestrate startup.
func TestDispatchUnknownCommand(t *testing.T) {
	err := dispatch([]string{"frobnicate"})
	if err == nil {
		t.Fatal("dispatch of an unknown command returned no error")
	}
	if !strings.Contains(err.Error(), "usage:") {
		t.Errorf("unknown-command error lacks usage hint: %v", err)
	}
}

// TestDispatchForthcomingCommands must give the not-yet-built commands an honest
// "not implemented" message instead of silently doing nothing useful.
func TestDispatchForthcomingCommands(t *testing.T) {
	for _, cmd := range []string{"discuss", "plan", "build"} {
		err := dispatch([]string{cmd})
		if err == nil || !strings.Contains(err.Error(), "not implemented") {
			t.Errorf("dispatch(%q) = %v, want a 'not implemented' error", cmd, err)
		}
	}
}

// TestInitAtWritesLoadableConfig must create a loadable, build-ready config under
// the project's .flanders/ and report that it wrote the file.
func TestInitAtWritesLoadableConfig(t *testing.T) {
	root := t.TempDir()
	var out bytes.Buffer
	if err := initAt(root, &out); err != nil {
		t.Fatalf("initAt: %v", err)
	}
	if !strings.Contains(out.String(), "wrote default config") {
		t.Errorf("init output = %q, want a 'wrote' message", out.String())
	}

	cfgPath := filepath.Join(root, ".flanders", "config.toml")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load init'd config: %v", err)
	}
	if err := cfg.ValidateForBuild(); err != nil {
		t.Errorf("init'd config failed ValidateForBuild: %v", err)
	}

	// init must also materialize the loop rules so the user can read/tune them.
	if !strings.Contains(out.String(), "wrote default loop rules") {
		t.Errorf("init output = %q, want a 'wrote default loop rules' message", out.String())
	}
	rulesPath := filepath.Join(root, ".flanders", "rules.md")
	gotRules, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatalf("read init'd rules: %v", err)
	}
	if string(gotRules) != rules.DefaultMarkdown {
		t.Errorf("init'd rules differ from the built-in default")
	}
}

// loadConfigOrDefault must fall back to documented defaults when no config file
// exists — a bare `flanders` before `init` has to run on defaults, not error.
func TestLoadConfigOrDefaultMissing(t *testing.T) {
	root := t.TempDir() // no .flanders/config.toml
	cfg, err := loadConfigOrDefault(root)
	if err != nil {
		t.Fatalf("loadConfigOrDefault on missing config: %v", err)
	}
	if cfg.Paths.Specs != "specs" || cfg.Agent.Bin != "claude" {
		t.Errorf("missing config did not fall back to Default(): %+v", cfg.Paths)
	}
}

// loadConfigOrDefault must load a present config and honor its overlaid values,
// so the [paths] section a user wrote actually reaches startup.
func TestLoadConfigOrDefaultPresent(t *testing.T) {
	root := t.TempDir()
	if err := initAt(root, &bytes.Buffer{}); err != nil {
		t.Fatalf("initAt: %v", err)
	}
	cfg, err := loadConfigOrDefault(root)
	if err != nil {
		t.Fatalf("loadConfigOrDefault on present config: %v", err)
	}
	// The init'd config sets the Go starter test command; defaults leave it empty.
	if cfg.Commands.Test == "" {
		t.Errorf("present config not loaded: Commands.Test empty, want the init starter")
	}
}

// A present-but-invalid config is a HARD error: the user asked for something we
// cannot honor, so we must not silently fall back to defaults.
func TestLoadConfigOrDefaultInvalid(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".flanders")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// guardrails.stall_n = 0 violates Validate (must be >= 1).
	bad := "[guardrails]\nstall_n = 0\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigOrDefault(root); err == nil {
		t.Error("loadConfigOrDefault on an invalid config returned no error")
	}
}

// TestInitAtIdempotent must not clobber an existing config on a second run, and
// must say so.
func TestInitAtIdempotent(t *testing.T) {
	root := t.TempDir()
	if err := initAt(root, &bytes.Buffer{}); err != nil {
		t.Fatalf("first initAt: %v", err)
	}

	var out bytes.Buffer
	if err := initAt(root, &out); err != nil {
		t.Fatalf("second initAt: %v", err)
	}
	if !strings.Contains(out.String(), "already exists") {
		t.Errorf("second init output = %q, want an 'already exists' message", out.String())
	}
}
