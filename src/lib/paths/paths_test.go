package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// New must produce absolute paths that match the documented [paths] defaults
// (specs/03-config.md) joined onto the root — this is the single source of
// truth every other package relies on.
func TestNewResolvesDefaults(t *testing.T) {
	root := t.TempDir()
	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := map[string]string{
		p.Specs:    filepath.Join(root, "specs"),
		p.Tasks:    filepath.Join(root, "specs", "tasks"),
		p.Journal:  filepath.Join(root, ".flanders", "journal"),
		p.Plan:     filepath.Join(root, "IMPLEMENTATION_PLAN.md"),
		p.State:    filepath.Join(root, ".flanders", "state.json"),
		p.Rules:    filepath.Join(root, ".flanders", "rules.md"),
		p.Config:   filepath.Join(root, ".flanders", "config.toml"),
		p.Log:      filepath.Join(root, ".flanders", "flanders.log"),
		p.Flanders: filepath.Join(root, ".flanders"),
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	}
	if p.Root != root {
		t.Errorf("Root = %q, want %q", p.Root, root)
	}
}

// New must return absolute paths even when given a relative root, so consumers
// never depend on the process working directory after construction.
func TestNewMakesRootAbsolute(t *testing.T) {
	p, err := New(".")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !filepath.IsAbs(p.Root) {
		t.Errorf("Root = %q, want absolute", p.Root)
	}
}

// EnsureFlanders creates .flanders/ and its journal subdir on demand and is
// idempotent — startup calls it unconditionally.
func TestEnsureFlanders(t *testing.T) {
	root := t.TempDir()
	p, _ := New(root)
	for i := 0; i < 2; i++ { // twice: must be idempotent
		if err := p.EnsureFlanders(); err != nil {
			t.Fatalf("EnsureFlanders (call %d): %v", i, err)
		}
	}
	for _, dir := range []string{p.Flanders, p.Journal} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %q: %v", dir, err)
		}
		if !info.IsDir() {
			t.Errorf("%q is not a directory", dir)
		}
	}
}

// FindRoot must locate the nearest ancestor carrying a project marker, walking
// up from a nested start directory.
func TestFindRootLocatesMarker(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := FindRoot(nested)
	if err != nil {
		t.Fatalf("FindRoot: %v", err)
	}
	// t.TempDir may live under a symlinked path (e.g. macOS /var); compare via
	// EvalSymlinks so the assertion is portable.
	wantResolved, _ := filepath.EvalSymlinks(root)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantResolved {
		t.Errorf("FindRoot = %q, want %q", got, root)
	}
}

// FindRoot must error when no marker exists anywhere up the tree, so callers
// can fall back deliberately rather than silently picking the wrong root.
func TestFindRootNoMarker(t *testing.T) {
	dir := t.TempDir() // an isolated temp tree with no markers above it
	if _, err := FindRoot(dir); err == nil {
		t.Errorf("FindRoot(%q) = nil error, want error (no marker)", dir)
	}
}
