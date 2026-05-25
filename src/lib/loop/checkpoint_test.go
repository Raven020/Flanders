package loop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"flanders/src/lib/git"
	"flanders/src/lib/stream"
	"flanders/src/lib/supervise"
	"flanders/src/lib/task"
)

// headSubject returns the subject line of dir's current HEAD commit, for asserting
// the templated checkpoint message.
func headSubject(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%s").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// flipStub returns a d.run that flips the task at taskPath to a terminal status
// (the agent's own mid-loop status edit, spec 02 §Mutation ownership) and reports a
// clean success — so the loop records a status change, the checkpoint's progress trigger.
func flipStub(t *testing.T, taskPath string, set func(*task.Task)) func(context.Context, supervise.Spec) (*supervise.Result, error) {
	return func(_ context.Context, _ supervise.Spec) (*supervise.Result, error) {
		tk, err := task.ParseFile(taskPath)
		if err != nil {
			t.Fatalf("agent reload: %v", err)
		}
		set(tk)
		if err := tk.WriteFile(taskPath); err != nil {
			t.Fatalf("agent write: %v", err)
		}
		return &supervise.Result{Observation: &stream.LoopObservation{Done: true, Subtype: "success"}, ExitCode: 0}, nil
	}
}

// TestCheckpointCommitsOnProgress is the headline 3.6 acceptance: in the default
// `progress` mode, a loop that changes the task's status produces a git commit whose
// message is the rendered [git].message_tmpl, and the new sha is surfaced on Result.
func TestCheckpointCommitsOnProgress(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Git.Enabled = true // re-enable (setupProject disables it for the other tests)
	initGitRepo(t, p.Root) // clean baseline commit
	base := git.HeadSHA(context.Background(), p.Root)

	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = flipStub(t, filepath.Join(p.Tasks, "0001.md"), func(tk *task.Task) { tk.SetStatus(task.StatusDone) })

	res, err := d.Iterate(context.Background(), "build", 3)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.Checkpoint == "" {
		t.Fatal("Result.Checkpoint = \"\", want a commit sha (progress was made)")
	}
	if now := git.HeadSHA(context.Background(), p.Root); now == base || now != res.Checkpoint {
		t.Errorf("HEAD = %q (base %q), Result.Checkpoint = %q; want HEAD advanced to the checkpoint", now, base, res.Checkpoint)
	}
	// The commit message is the rendered default template: "Flanders: {phase} #{iter} — {task} [{result}]".
	if got, want := headSubject(t, p.Root), "Flanders: build #3 — 0001 [done]"; got != want {
		t.Errorf("commit subject = %q, want %q", got, want)
	}
	// The checkpoint left the tree clean (revertable iterations; clean diff next loop).
	if ch, _ := git.WorkingChanges(context.Background(), p.Root); len(ch) != 0 {
		t.Errorf("working tree not clean after checkpoint: %v", ch)
	}
}

// TestCheckpointCommitsOnPassingGate: the OTHER progress trigger. The agent leaves
// the task pending, but the harness test gate passes, so reconcile promotes it to
// done and the checkpoint fires — proving the commit follows the test ground truth,
// not only an agent flip.
func TestCheckpointCommitsOnPassingGate(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Git.Enabled = true
	cfg.Commands.Test = "exit 0" // gate passes → reconcile promotes pending→done
	initGitRepo(t, p.Root)
	base := git.HeadSHA(context.Background(), p.Root)

	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = stubSuccess // agent does NOT flip status; the gate does the work

	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.Checkpoint == "" || git.HeadSHA(context.Background(), p.Root) == base {
		t.Fatalf("want a checkpoint commit on a passing gate; Checkpoint=%q HEAD moved=%v",
			res.Checkpoint, git.HeadSHA(context.Background(), p.Root) != base)
	}
	if got, want := headSubject(t, p.Root), "Flanders: build #1 — 0001 [done]"; got != want {
		t.Errorf("commit subject = %q, want %q", got, want)
	}
}

// TestCheckpointSkipsWithoutProgress: in `progress` mode, a loop that edits files but
// neither changes status nor passes a gate makes NO commit — the working changes stay
// uncommitted for a later loop to finish, matching "commit only on real progress".
func TestCheckpointSkipsWithoutProgress(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Git.Enabled = true // progress mode (default); no [commands].test → gate does not run
	initGitRepo(t, p.Root)
	base := git.HeadSHA(context.Background(), p.Root)

	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = func(_ context.Context, _ supervise.Spec) (*supervise.Result, error) {
		if err := os.WriteFile(filepath.Join(p.Root, "wip.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatalf("agent write: %v", err)
		}
		return &supervise.Result{Observation: &stream.LoopObservation{Done: true, Subtype: "success"}, ExitCode: 0}, nil
	}

	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.Checkpoint != "" {
		t.Errorf("Result.Checkpoint = %q, want \"\" (no progress in progress mode)", res.Checkpoint)
	}
	if git.HeadSHA(context.Background(), p.Root) != base {
		t.Error("HEAD advanced without progress, want it unchanged")
	}
	if ch, _ := git.WorkingChanges(context.Background(), p.Root); len(ch) == 0 {
		t.Error("the loop's file edit was committed away, want it left uncommitted")
	}
}

// TestCheckpointIterationModeCommitsAnyChange: with commit_each="iteration" the
// harness commits every loop that touched the tree, even with no status change.
func TestCheckpointIterationModeCommitsAnyChange(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Git.Enabled = true
	cfg.Git.CommitEach = "iteration"
	initGitRepo(t, p.Root)
	base := git.HeadSHA(context.Background(), p.Root)

	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = func(_ context.Context, _ supervise.Spec) (*supervise.Result, error) {
		if err := os.WriteFile(filepath.Join(p.Root, "wip.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatalf("agent write: %v", err)
		}
		return &supervise.Result{Observation: &stream.LoopObservation{Done: true, Subtype: "success"}, ExitCode: 0}, nil
	}

	res, err := d.Iterate(context.Background(), "build", 7)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.Checkpoint == "" || git.HeadSHA(context.Background(), p.Root) == base {
		t.Errorf("iteration mode: want a commit even without status change; Checkpoint=%q", res.Checkpoint)
	}
}

// TestCheckpointOffNeverCommits: commit_each="off" disables checkpointing even on
// clear progress.
func TestCheckpointOffNeverCommits(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Git.Enabled = true
	cfg.Git.CommitEach = "off"
	initGitRepo(t, p.Root)
	base := git.HeadSHA(context.Background(), p.Root)

	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = flipStub(t, filepath.Join(p.Tasks, "0001.md"), func(tk *task.Task) { tk.SetStatus(task.StatusDone) })

	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.Checkpoint != "" || git.HeadSHA(context.Background(), p.Root) != base {
		t.Errorf("commit_each=off still committed: Checkpoint=%q", res.Checkpoint)
	}
}

// TestCheckpointDisabledNeverCommits: [git].enabled=false disables the whole feature,
// independent of commit_each.
func TestCheckpointDisabledNeverCommits(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Git.Enabled = false // the default in setupProject, made explicit here
	initGitRepo(t, p.Root)
	base := git.HeadSHA(context.Background(), p.Root)

	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = flipStub(t, filepath.Join(p.Tasks, "0001.md"), func(tk *task.Task) { tk.SetStatus(task.StatusDone) })

	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.Checkpoint != "" || git.HeadSHA(context.Background(), p.Root) != base {
		t.Errorf("git.enabled=false still committed: Checkpoint=%q", res.Checkpoint)
	}
}

// TestCheckpointInitIfMissing: a non-repo target plus init_if_missing=true means the
// harness initializes a repo (the autonomous-run consent) and commits the loop's
// progress into it. Identity is pinned via env so the commit is deterministic without
// relying on the machine's global git config.
func TestCheckpointInitIfMissing(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "Flanders Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@flanders.local")
	t.Setenv("GIT_COMMITTER_NAME", "Flanders Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@flanders.local")

	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Git.Enabled = true
	cfg.Git.InitIfMissing = true
	if git.IsRepo(context.Background(), p.Root) {
		t.Fatal("precondition: target should not be a repo yet")
	}

	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = flipStub(t, filepath.Join(p.Tasks, "0001.md"), func(tk *task.Task) { tk.SetStatus(task.StatusDone) })

	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if !git.IsRepo(context.Background(), p.Root) {
		t.Error("target is still not a repo, want the harness to have git-init'd it")
	}
	if res.Checkpoint == "" {
		t.Error("Result.Checkpoint = \"\", want a commit into the freshly-initialized repo")
	}
}

// TestCheckpointInitDisabledNoCommit: a non-repo target with init_if_missing=false is
// left untouched — no repo is created and no checkpoint is made.
func TestCheckpointInitDisabledNoCommit(t *testing.T) {
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	cfg.Git.Enabled = true
	cfg.Git.InitIfMissing = false

	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.run = flipStub(t, filepath.Join(p.Tasks, "0001.md"), func(tk *task.Task) { tk.SetStatus(task.StatusDone) })

	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	if res.Checkpoint != "" {
		t.Errorf("Result.Checkpoint = %q, want \"\" (no repo, init disabled)", res.Checkpoint)
	}
	if git.IsRepo(context.Background(), p.Root) {
		t.Error("a repo was created with init_if_missing=false, want none")
	}
}

// TestRenderCheckpointMessage covers the template variable substitution and that an
// unknown {placeholder} in a custom template is left verbatim (visible, not dropped).
func TestRenderCheckpointMessage(t *testing.T) {
	got := renderCheckpointMessage("Flanders: {phase} #{iter} — {task} [{result}]", "build", 12, "0007", "blocked")
	if want := "Flanders: build #12 — 0007 [blocked]"; got != want {
		t.Errorf("render = %q, want %q", got, want)
	}
	got = renderCheckpointMessage("{task}:{result} ({unknown})", "plan", 1, "0001", "done")
	if want := "0001:done ({unknown})"; got != want {
		t.Errorf("render with unknown placeholder = %q, want %q", got, want)
	}
}
