package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestDefaultTOMLMatchesDefault is the anti-drift lock: the commented file
// `flanders init` writes must load to exactly Default() — apart from the
// [commands] starters it intentionally fills (test/build have no overlay default;
// see write.go). If someone changes a documented default without updating the
// template (or vice-versa), this fails. It also proves the generated file is
// loadable and build-ready (the acceptance for task 1.2).
func TestDefaultTOMLMatchesDefault(t *testing.T) {
	got, err := Load(writeConfig(t, DefaultTOML))
	if err != nil {
		t.Fatalf("Load(DefaultTOML): %v", err)
	}

	want := Default()
	want.Commands.Test = "go test ./..."   // starter the template fills (Default leaves "")
	want.Commands.Build = "go build ./..." // starter the template fills (Default leaves "")

	if !reflect.DeepEqual(*got, want) {
		t.Errorf("DefaultTOML does not load to Default()+starters:\n got = %+v\nwant = %+v", *got, want)
	}

	// A freshly-init'd config must satisfy the build gate out of the box.
	if err := got.ValidateForBuild(); err != nil {
		t.Errorf("DefaultTOML failed ValidateForBuild: %v", err)
	}
	// No accidental subagent overrides leak from the commented example.
	if len(got.Subagents.Classes) != 0 {
		t.Errorf("DefaultTOML parsed unexpected subagent classes: %+v", got.Subagents.Classes)
	}
}

// TestWriteDefaultCreates must write the template into a fresh path (creating the
// parent .flanders/ dir) and report wrote=true, with on-disk bytes == DefaultTOML.
func TestWriteDefaultCreates(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".flanders", "config.toml")

	wrote, err := WriteDefault(path)
	if err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}
	if !wrote {
		t.Fatal("WriteDefault reported wrote=false for an absent file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	if string(data) != DefaultTOML {
		t.Errorf("written bytes differ from DefaultTOML")
	}
}

// TestWriteDefaultDoesNotOverwrite must leave an existing config untouched and
// report wrote=false — init is for the missing-config case, not regeneration.
func TestWriteDefaultDoesNotOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	const sentinel = "# user-edited, do not touch\n"
	if err := os.WriteFile(path, []byte(sentinel), 0o644); err != nil {
		t.Fatalf("seed existing config: %v", err)
	}

	wrote, err := WriteDefault(path)
	if err != nil {
		t.Fatalf("WriteDefault over existing: %v", err)
	}
	if wrote {
		t.Error("WriteDefault reported wrote=true over an existing file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(data) != sentinel {
		t.Errorf("WriteDefault clobbered an existing config: got %q", string(data))
	}
}

// TestWriteDefaultLeavesNoTempFiles must clean up its temp file after a
// successful atomic write (no .config-*.tmp residue beside the final file).
func TestWriteDefaultLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if _, err := WriteDefault(path); err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover temp file after write: %s", e.Name())
		}
	}
}

// TestWriteDefaultEmptyPath must reject an empty path rather than write to ".".
func TestWriteDefaultEmptyPath(t *testing.T) {
	if _, err := WriteDefault(""); err == nil {
		t.Error("WriteDefault(\"\") returned no error")
	}
}
