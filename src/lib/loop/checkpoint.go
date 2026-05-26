package loop

import (
	"context"
	"strconv"
	"strings"

	"flanders/src/lib/git"
	"flanders/src/lib/task"
	"flanders/src/lib/verify"
)

// checkpoint is step 7 of the iteration anatomy (spec 01 §Checkpointing): on
// progress, the harness makes a git commit so iterations are revertable. It returns
// the new commit sha — which the orchestrator (Phase 5) persists to
// state.last_checkpoint (spec 09) — or "" when no commit was made this loop.
//
// "Progress" follows the spec exactly (spec 03 [git].commit_each): a status change
// OR a passing test gate. The selector always hands the loop a `pending` task, so a
// status change is simply before.Status() != after.Status() (a flip to done/blocked,
// or a harness promotion/normalization in src/lib/reconcile). The commit_each modes:
//   - "progress"  — commit only on progress (the default; matches the checkpoint rule)
//   - "iteration" — commit every loop that touched the tree (progress is forced true)
//   - "off"       — never commit; likewise when [git].enabled is false
//
// Why best-effort, never fatal. The loop's journal entry is already written and the
// work is already on disk before this step runs, so a failed commit — most often a
// target repo with no configured commit identity — must not abort the run or lose the
// iteration. A failure is logged and yields "", and the loop proceeds, mirroring the
// read-side git signal's best-effort contract (src/lib/git package doc).
//
// init_if_missing. When checkpointing is wanted but the target is not a repo, the
// [git].init_if_missing flag is the standing consent for an autonomous run to
// `git init` it (spec 01 §Checkpointing: "if not, the harness offers to git init";
// spec 03). An interactive offer is a TUI concern (Phase 6); at the engine level the
// config flag governs, so an unattended run is never blocked waiting for an answer.
func (d *Driver) checkpoint(ctx context.Context, phase string, iter int, before, after *task.Task, vr *verify.Result) string {
	// Progress for a task loop (spec 03 [git].commit_each): a status change OR a passing
	// test gate. The selector always hands a `pending` task, so a status change is simply
	// before.Status() != after.Status() (a flip to done/blocked, or a harness
	// promotion/normalization in src/lib/reconcile).
	statusChanged := before.Status() != after.Status()
	testsPassed := vr != nil && vr.Passed()
	return d.commit(ctx, phase, iter, after.ID(), string(after.Status()), statusChanged || testsPassed)
}

// commit is the checkpoint core shared by the task loop (checkpoint) and the plan loop
// (planCheckpoint). It honors [git].enabled/commit_each, ensures a repo
// (init_if_missing), renders the [git].message_tmpl, and commits the working tree —
// returning the new commit sha, or "" when no commit was made. The only thing that
// differs between the two loops is what counts as "progress" (a task-status flip vs.
// new/changed task files) and the {task}/{result} message variables, so those are
// passed in; everything else (the modes, repo-init, best-effort error handling) is one
// implementation here. progress gates commit_each="progress"; "iteration" commits any
// tree change regardless; "off" (or [git].enabled=false) never commits.
func (d *Driver) commit(ctx context.Context, phase string, iter int, taskID, result string, progress bool) string {
	g := d.cfg.Git
	if !g.Enabled || g.CommitEach == "off" {
		return ""
	}
	if g.CommitEach == "progress" && !progress {
		return ""
	}

	// Ensure there is a repo to commit into. init_if_missing is the autonomous-run
	// consent to create one; without it, no repo means no checkpoint (lean on the
	// journal alone). A failed init is non-fatal — log and skip the commit.
	if !git.IsRepo(ctx, d.paths.Root) {
		if !g.InitIfMissing {
			d.log.Debug("checkpoint skipped: target is not a git repo and init_if_missing is false",
				"root", d.paths.Root)
			return ""
		}
		if err := git.Init(ctx, d.paths.Root); err != nil {
			d.log.Warn("checkpoint: git init failed", "root", d.paths.Root, "err", err)
			return ""
		}
		d.log.Info("checkpoint: initialized git repo for target", "root", d.paths.Root)
	}

	msg := renderCheckpointMessage(g.MessageTmpl, phase, iter, taskID, result)
	sha, committed, err := git.Checkpoint(ctx, d.paths.Root, msg)
	if err != nil {
		d.log.Warn("checkpoint: commit failed", "phase", phase, "task", taskID, "err", err)
		return ""
	}
	if !committed {
		// Progress was recorded, but the tree was already clean — e.g. the agent
		// committed its own work. Not a failure; just nothing left to do.
		d.log.Debug("checkpoint: nothing to commit", "phase", phase, "task", taskID)
		return ""
	}
	d.log.Info("checkpoint committed", "phase", phase, "task", taskID, "sha", sha, "message", msg)
	return sha
}

// renderCheckpointMessage fills the [git].message_tmpl variables (spec 03): {phase},
// {iter}, {task}, {result}. The default template is
// "Flanders: {phase} #{iter} — {task} [{result}]". {result} is the task's resulting
// status (done|blocked|…) — the most useful one-word loop outcome for a commit log.
// Unknown {placeholders} in a user's custom template are left verbatim rather than
// blanked, so a typo is visible in the commit message instead of silently dropped.
func renderCheckpointMessage(tmpl, phase string, iter int, taskID, result string) string {
	return strings.NewReplacer(
		"{phase}", phase,
		"{iter}", strconv.Itoa(iter),
		"{task}", taskID,
		"{result}", result,
	).Replace(tmpl)
}
