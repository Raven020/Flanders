// Package git is the harness's read-only window onto the target project's git
// working tree. It answers two questions the harness owns and must never delegate
// to the agent (specs/02-plan-and-tasks.md §Mutation ownership, spec
// 09-state-and-resume.md §resume): "did work actually happen this loop?" (the
// git-diff signal behind status reconciliation and the stall guardrail) and "which
// files were touched?" (recorded in the journal).
//
// Why reads only. Committing, staging, and `git init` are a separate, write-side
// concern (checkpointing — plan task 3.6). Keeping them out of this package means a
// status-inference path can never accidentally mutate the very repo it is
// measuring — the measurement and the commit have different owners by construction.
//
// Why best-effort, never fatal. Every call is scoped to an explicit dir (via
// `git -C <dir>`) so the harness can drive a target that lives anywhere, and a
// missing git binary or a non-repo target yields "no signal" rather than an error
// on the hot path of every loop. Callers gate on [IsRepo] (or read [Snapshot.IsRepo])
// and simply lean on the test gate alone when git can say nothing; the orchestrator
// may then offer `git init` (spec 03 [git].init_if_missing, task 3.6).
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
