package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"flanders/src/lib/config"
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
