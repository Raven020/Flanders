// Package git is the harness's window onto the target project's git working tree.
// It has two clearly separated sides:
//
//   - A READ side (the original purpose): answering two questions the harness owns
//     and must never delegate to the agent (specs/02-plan-and-tasks.md §Mutation
//     ownership, spec 09-state-and-resume.md §resume): "did work actually happen this
//     loop?" ([Diff]'s workHappened — the signal behind status reconciliation and the
//     stall guardrail) and "which files were touched?" ([Diff]'s files — recorded in
//     the journal). [IsRepo]/[HeadSHA]/[WorkingChanges]/[Snap]/[Diff] never mutate.
//   - A WRITE side (checkpointing — plan task 3.6): [Init]/[AddAll]/[Commit] and the
//     [Checkpoint] convenience that commits the loop's work so Ralph iterations are
//     revertable (spec 01 §Checkpointing, step 7 of the loop anatomy).
//
// Why one package, two sides. The split is by FUNCTION, not by file: status
// reconciliation calls only the read side, so a status-inference path still can never
// accidentally mutate the repo it is measuring — that ownership boundary is enforced
// at the call sites (src/lib/reconcile imports task only; the loop driver reads before
// the loop and writes the checkpoint after). Keeping both in one `git` package means a
// single, audited shell-out helper ([run]) and no second copy of the same conventions.
//
// Why best-effort, never fatal. Every call is scoped to an explicit dir (via
// `git -C <dir>`) so the harness can drive a target that lives anywhere, and a
// missing git binary or a non-repo target yields "no signal" rather than an error
// on the hot path of every loop. Read callers gate on [IsRepo] (or read
// [Snapshot.IsRepo]) and lean on the test gate alone when git can say nothing; the
// checkpoint caller treats a commit failure as a logged, non-fatal event (the work is
// already on disk and journaled) and may `git init` first when [Init] is permitted
// (spec 03 [git].init_if_missing).
package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// run executes `git -C dir <args...>` and returns trimmed stdout. A non-zero exit
// (or a missing git binary) becomes an error with git's stderr attached, so a
// caller can treat "git unavailable / not a repo" as "no signal" instead of
// crashing. The dir is passed via -C rather than cmd.Dir so the message names it.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// IsRepo reports whether dir is inside a git working tree. It is the guard the
// other calls sit behind: a false here means "no git signal available," not a
// failure — a target project need not be a repo until checkpointing wants one.
func IsRepo(ctx context.Context, dir string) bool {
	out, err := run(ctx, dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && out == "true"
}

// HeadSHA returns the current HEAD commit hash, or "" when there is no HEAD to
// read — an unborn branch (a fresh `git init` with no commits yet) or a non-repo.
// `--verify --quiet` makes rev-parse exit non-zero silently in those cases, so the
// empty string is the single, unambiguous "no commit" answer the caller needs
// (HEAD movement between snapshots is how [Diff] notices the loop committed).
func HeadSHA(ctx context.Context, dir string) string {
	out, err := run(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return ""
	}
	return out
}

// WorkingChanges returns the set of paths that differ from a clean HEAD — modified,
// staged, deleted, renamed, and untracked files — from `git status --porcelain`.
// A set (map) is returned because the only questions asked of it are membership and
// equality between two snapshots ([Diff]); an empty map means a clean tree.
func WorkingChanges(ctx context.Context, dir string) (map[string]struct{}, error) {
	out, err := run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	changes := map[string]struct{}{}
	for _, line := range strings.Split(out, "\n") {
		if p := parsePorcelainPath(line); p != "" {
			changes[p] = struct{}{}
		}
	}
	return changes, nil
}

// parsePorcelainPath extracts the path from one porcelain-v1 line. The format is
// two status columns, a space, then the path (`XY path`); a rename is rendered
// `R  old -> new`, where the new name is the one that now exists on disk. Empty or
// too-short lines (e.g. the trailing newline split) yield "".
func parsePorcelainPath(line string) string {
	if len(line) < 4 {
		return ""
	}
	p := strings.TrimSpace(line[3:])
	if i := strings.Index(p, " -> "); i >= 0 {
		p = p[i+len(" -> "):] // rename/copy: the destination is what was touched
	}
	return strings.Trim(p, `"`) // core.quotepath may wrap unusual names in quotes
}

// Snapshot is the git state of a working tree at one instant: the HEAD commit and
// the set of paths differing from it. The harness takes one before a loop and one
// after, then compares them ([Diff]) to decide whether the loop landed work.
type Snapshot struct {
	Head    string              // HEAD sha; "" for an unborn branch / non-repo
	Changes map[string]struct{} // working-tree changes (see [WorkingChanges])
	IsRepo  bool                // false ⇒ no git signal was available
}

// Snap captures the current working-tree state of dir. It never errors: a non-repo
// target (or an unavailable git) yields a zero-value Snapshot with IsRepo=false, so
// the caller gets "no signal" rather than an error to handle on every loop.
func Snap(ctx context.Context, dir string) Snapshot {
	if !IsRepo(ctx, dir) {
		return Snapshot{}
	}
	s := Snapshot{IsRepo: true, Head: HeadSHA(ctx, dir)}
	if ch, err := WorkingChanges(ctx, dir); err == nil {
		s.Changes = ch
	}
	return s
}

// Diff reports whether work landed between two snapshots of the same tree and the
// sorted list of paths newly touched. "Work happened" is true when HEAD advanced
// (the loop committed — uncommon, since the harness owns commits) OR the dirty-file
// set changed (the agent edited the tree, or a path went clean). Newly-touched
// files are those dirty after but not before — which, once checkpointing (task 3.6)
// cleans the tree between loops, is exactly the loop's own changes. A non-repo
// after-snapshot means no signal: (false, nil).
func Diff(before, after Snapshot) (workHappened bool, files []string) {
	if !after.IsRepo {
		return false, nil
	}
	if before.Head != after.Head {
		workHappened = true
	}
	for p := range after.Changes {
		if _, had := before.Changes[p]; !had {
			files = append(files, p)
			workHappened = true
		}
	}
	if !workHappened {
		// No new dirty path, but a path may have gone clean (committed/reverted) —
		// that is still work, so check the other direction before concluding "none".
		for p := range before.Changes {
			if _, still := after.Changes[p]; !still {
				workHappened = true
				break
			}
		}
	}
	sort.Strings(files)
	return workHappened, files
}

// Init initializes a new git repository in dir (`git init`). It is the write-side
// counterpart to [IsRepo] for the checkpoint step: when the target project is not yet
// a repo and the config permits (spec 03 [git].init_if_missing), the harness creates
// one so iterations become revertable (spec 01 §Checkpointing). `git init` is
// idempotent, but callers gate on [IsRepo] to avoid the needless work and the log noise.
func Init(ctx context.Context, dir string) error {
	_, err := run(ctx, dir, "init", "-q")
	return err
}

// AddAll stages every change in the working tree (`git add -A`) — modifications, new
// (untracked) files, and deletions — so the next [Commit] captures the loop's full
// output. The harness checkpoints the whole tree, not a curated subset: a Ralph
// iteration's unit of work is "whatever this loop changed," and a partial stage could
// leave the commit out of step with the journal's recorded file list.
func AddAll(ctx context.Context, dir string) error {
	_, err := run(ctx, dir, "add", "-A")
	return err
}

// Commit records the currently staged changes with message and returns the new HEAD
// sha. It assumes something is staged; a clean index makes `git commit` exit non-zero
// ("nothing to commit"), surfaced here as an error — [Checkpoint] guards against that
// so the no-progress case is never mistaken for a failure. A missing commit identity
// (no user.name/email configured anywhere) is likewise a real, returned error.
func Commit(ctx context.Context, dir, message string) (string, error) {
	if _, err := run(ctx, dir, "commit", "-q", "-m", message); err != nil {
		return "", err
	}
	return HeadSHA(ctx, dir), nil
}

// Checkpoint stages all working-tree changes and commits them with message, returning
// the new commit sha. It is the harness's revertable-iteration commit (spec 01
// §Checkpointing, step 7 of the loop anatomy). A clean tree — the agent already
// committed, or the loop changed nothing — is a NO-OP, not a failure: it returns
// (sha="", committed=false, err=nil) so the caller never mistakes "no changes" for
// "commit broke". Any real git failure (e.g. no commit identity) is returned. This
// also leaves the tree clean for the next loop, which is what makes [Diff]'s
// newly-touched-file set exactly that next loop's own changes.
func Checkpoint(ctx context.Context, dir, message string) (sha string, committed bool, err error) {
	if err := AddAll(ctx, dir); err != nil {
		return "", false, err
	}
	// After `add -A`, `git status --porcelain` reports the staged changes; an empty
	// result means the tree was already clean (incl. a fresh, file-less repo), so
	// there is nothing to commit. This is cheaper and clearer than committing and
	// parsing the "nothing to commit" error back out.
	changes, err := WorkingChanges(ctx, dir)
	if err != nil {
		return "", false, err
	}
	if len(changes) == 0 {
		return "", false, nil
	}
	sha, err = Commit(ctx, dir, message)
	if err != nil {
		return "", false, err
	}
	return sha, true, nil
}
