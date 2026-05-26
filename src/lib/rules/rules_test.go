package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The rules text is appended to every loop's system prompt, so an accidental
// deletion of one of the five behavioral rules would silently weaken the agent's
// contract on every future loop. This anti-drift lock asserts each required
// behavior (specs/01 §invocation/§prompt-composition/§guardrails tier 1; specs/02
// §mutation ownership) still appears in DefaultMarkdown.
func TestDefaultMarkdownCoversRequiredRules(t *testing.T) {
	required := []struct {
		concept string
		needles []string // any one present satisfies the concept
	}{
		{"one task per loop", []string{"One unit of work per loop", "one task per loop"}},
		{"flip own status", []string{"status", "blocked", "done"}},
		{"don't hand-edit harness state", []string{".flanders/state.json", ".flanders/journal/"}},
		{"delegate to subagents", []string{"subagent"}},
		{"context-overreach handoff", []string{"context-overreach"}},
	}
	for _, r := range required {
		ok := false
		for _, n := range r.needles {
			if strings.Contains(DefaultMarkdown, n) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("DefaultMarkdown is missing the %q rule (none of %v found)", r.concept, r.needles)
		}
	}
}

func TestWriteDefaultCreates(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".flanders", "rules.md")
	wrote, err := WriteDefault(path)
	if err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}
	if !wrote {
		t.Fatal("WriteDefault returned wrote=false for a missing file")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written rules: %v", err)
	}
	if string(got) != DefaultMarkdown {
		t.Errorf("written rules differ from DefaultMarkdown")
	}
}

// WriteDefault must never clobber a user's tuned rules: a second call leaves the
// existing content untouched and reports wrote=false without an error.
func TestWriteDefaultNoOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.md")
	custom := "# my rules\n"
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	wrote, err := WriteDefault(path)
	if err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}
	if wrote {
		t.Error("WriteDefault overwrote an existing rules file")
	}
	got, _ := os.ReadFile(path)
	if string(got) != custom {
		t.Errorf("existing rules were modified: %q", got)
	}
}

// The atomic temp-then-rename write must leave no .tmp residue behind on success.
func TestWriteDefaultNoTempResidue(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteDefault(filepath.Join(dir, "rules.md")); err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") || strings.HasPrefix(e.Name(), ".rules-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestWriteDefaultEmptyPath(t *testing.T) {
	if _, err := WriteDefault(""); err == nil {
		t.Error("WriteDefault(\"\") returned no error")
	}
}
