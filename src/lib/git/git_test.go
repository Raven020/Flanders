package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initRepo creates a git repo in a fresh temp dir with a deterministic identity
// (so commits work in CI with no global git config) and returns its path.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@flanders.local"},
		{"config", "user.name", "Flanders Test"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func gitDo(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestIsRepo: a fresh git dir is a repo; a bare temp dir is not. This is the guard
// every other call sits behind, so it must be exact.
func TestIsRepo(t *testing.T) {
	ctx := context.Background()
	if !IsRepo(ctx, initRepo(t)) {
		t.Error("IsRepo on a git dir = false, want true")
	}
	if IsRepo(ctx, t.TempDir()) {
		t.Error("IsRepo on a non-git dir = true, want false")
	}
}

// TestHeadSHA: an unborn branch (no commits) reads as "", and a real commit reads
// as a non-empty hash — the HEAD-movement signal Diff keys off.
func TestHeadSHA(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	if sha := HeadSHA(ctx, dir); sha != "" {
		t.Errorf("HeadSHA on an unborn branch = %q, want \"\"", sha)
	}
	write(t, dir, "a.txt", "hello\n")
	gitDo(t, dir, "add", "a.txt")
	gitDo(t, dir, "commit", "-qm", "first")
	if sha := HeadSHA(ctx, dir); sha == "" {
		t.Error("HeadSHA after a commit = \"\", want a hash")
	}
}

// TestWorkingChanges: a clean tree has no changes; an untracked then a modified
// file shows up; a commit clears it.
func TestWorkingChanges(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)

	ch, err := WorkingChanges(ctx, dir)
	if err != nil {
		t.Fatalf("WorkingChanges: %v", err)
	}
	if len(ch) != 0 {
		t.Errorf("clean tree has %d changes, want 0: %v", len(ch), ch)
	}

	write(t, dir, "a.txt", "hello\n")
	ch, _ = WorkingChanges(ctx, dir)
	if _, ok := ch["a.txt"]; !ok || len(ch) != 1 {
		t.Errorf("after creating a.txt, changes = %v, want {a.txt}", ch)
	}

	gitDo(t, dir, "add", "a.txt")
	gitDo(t, dir, "commit", "-qm", "first")
	ch, _ = WorkingChanges(ctx, dir)
	if len(ch) != 0 {
		t.Errorf("after commit, changes = %v, want none", ch)
	}
}

// TestSnapDiffDetectsWork is the headline: a snapshot before and after a file edit
// reports work happened and names the touched file; with no edit between snapshots
// it reports none. This is the signal status reconciliation and the stall guardrail
// consume.
func TestSnapDiffDetectsWork(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	write(t, dir, "a.txt", "x\n")
	gitDo(t, dir, "add", "a.txt")
	gitDo(t, dir, "commit", "-qm", "base")

	before := Snap(ctx, dir)
	if !before.IsRepo {
		t.Fatal("Snap on a repo: IsRepo=false")
	}

	// No change between snapshots → no work.
	if work, files := Diff(before, Snap(ctx, dir)); work || files != nil {
		t.Errorf("Diff with no change = (%v, %v), want (false, nil)", work, files)
	}

	// Touch a new file → work happened, file named.
	write(t, dir, "b.txt", "y\n")
	work, files := Diff(before, Snap(ctx, dir))
	if !work {
		t.Error("Diff after touching b.txt: workHappened=false, want true")
	}
	if len(files) != 1 || files[0] != "b.txt" {
		t.Errorf("Diff files = %v, want [b.txt]", files)
	}
}

// TestDiffNoticesCommit: even with a clean working tree on both sides, an advanced
// HEAD (the loop committed) counts as work — the harness owns commits, but the
// signal must be robust if the agent ever does.
func TestDiffNoticesCommit(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	write(t, dir, "a.txt", "x\n")
	gitDo(t, dir, "add", "a.txt")
	gitDo(t, dir, "commit", "-qm", "base")

	before := Snap(ctx, dir)
	write(t, dir, "a.txt", "x\ny\n")
	gitDo(t, dir, "add", "a.txt")
	gitDo(t, dir, "commit", "-qm", "second")

	if work, _ := Diff(before, Snap(ctx, dir)); !work {
		t.Error("Diff after a commit (clean tree) = false, want true (HEAD moved)")
	}
}

// TestSnapNonRepoIsInert: a non-repo target yields an inert snapshot, so Diff says
// "no signal" instead of erroring on the hot path of every loop.
func TestSnapNonRepoIsInert(t *testing.T) {
	s := Snap(context.Background(), t.TempDir())
	if s.IsRepo {
		t.Error("Snap on a non-repo: IsRepo=true")
	}
	if work, files := Diff(s, s); work || files != nil {
		t.Errorf("Diff of inert snapshots = (%v, %v), want (false, nil)", work, files)
	}
}

// TestParsePorcelainPath covers the rename arrow and short/blank lines.
func TestParsePorcelainPath(t *testing.T) {
	cases := map[string]string{
		" M a.txt":           "a.txt",
		"?? new.txt":         "new.txt",
		"R  old.txt -> n.txt": "n.txt",
		"":                   "",
		"M":                  "",
	}
	for line, want := range cases {
		if got := parsePorcelainPath(line); got != want {
			t.Errorf("parsePorcelainPath(%q) = %q, want %q", line, got, want)
		}
	}
}
