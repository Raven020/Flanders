// Package rules is the single source of truth for the loop-rules text that the
// harness appends to every agent invocation's system prompt (specs/01-ralph-loop.md
// §Agent invocation: `--append-system-prompt` injects the loop rules).
//
// Why these live in code, materialized to disk by `flanders init`. The rules are
// the agent's behavioral contract for one Ralph iteration — "one unit of work per
// loop, flip your own status, don't hand-edit harness state, delegate exploration
// to subagents, hand off proactively on overreach" (specs/01 §invocation +
// §prompt-composition + §guardrails tier 1; specs/02 §mutation ownership). They
// MUST always be in force, so DefaultMarkdown is the built-in default the loop
// falls back to when no file is present (mirroring config.Default()'s philosophy:
// absence means "use the documented default," never "run with no rules"). `flanders
// init` writes the same text to .flanders/rules.md so a user can read and tune it;
// the loop reads that file when it exists and this default otherwise.
//
// Keeping the canonical text here (not in the loop package, nor inlined in init)
// means both the consumer (src/lib/loop.readRules) and the writer (cmd/flanders
// init) reference one string — no drift between "what we ship" and "what we run."
package rules

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultMarkdown is the canonical loop-rules text. It is appended verbatim to the
// agent's system prompt every loop, so it is written to be authoritative and lean:
// each rule states the behavior and the why (a rule the agent understands is a rule
// it follows). Edits here change agent behavior on the very next loop — keep it
// faithful to specs/01 and specs/02, and keep it short (it costs tokens per loop).
const DefaultMarkdown = `# Flanders loop rules

You are one iteration of a Ralph loop. The harness — not you — owns the loop, task
selection, the stopping decision, and ground truth (it runs the real test command).
These rules keep each loop a clean, focused, single-pass unit of work. Follow them.

## 1. One unit of work per loop

Do exactly one task per loop, then exit. You were handed one task and only the
context it needs. Finish that task and stop — a fresh session handles the next one,
so there is nothing to gain by reaching beyond this task.

## 2. Flip your task's ` + "`status`" + ` — it is your one structured edit

The task file in ` + "`specs/tasks/`" + ` is the source of truth. Update its frontmatter
` + "`status`" + ` as you work:

- ` + "`active`" + ` when you start,
- ` + "`done`" + ` when the acceptance criterion is met,
- ` + "`blocked`" + ` with a ` + "`reason`" + ` when you cannot finish (see rule 5).

` + "`reason`" + ` is required for ` + "`blocked`" + `; use the taxonomy
` + "`context-overreach | new-scope | dependency | error`" + `. You may also add a short
` + "`notes:`" + ` line and a ` + "`files:`" + ` list. Do not invent other frontmatter keys.

Your ` + "`done`" + ` is advisory: the harness runs the canonical test command itself and
decides completion. Write the status honestly anyway — it is the human-readable
record and the harness cross-checks against it.

## 3. Never hand-edit harness-owned state

Do not touch ` + "`.flanders/state.json`" + `, anything under ` + "`.flanders/journal/`" + `, or
` + "`IMPLEMENTATION_PLAN.md`" + ` (a checklist the harness regenerates from the task files).
Editing them corrupts the run's bookkeeping. Your writable surface is the project's
own source and tests, plus your task file in ` + "`specs/tasks/`" + `.

## 4. Delegate exploration and search to subagents

When you need to find code, read across many files, or investigate something, spawn
a subagent to do it and return only the conclusion. A subagent's context does NOT
count against your window — this is the primary lever for keeping your own context
lean and finishing in one pass. Pull a large file into your own context only when
you actually have to edit it.

## 5. Hand off proactively if the task is too big

If you realize the task is bigger than one focused pass — more than you can finish
well before your context fills — stop early instead of running deep. Near the window
limit quality degrades and auto-compaction can mangle the work. Set
` + "`status: blocked`" + `, ` + "`reason: context-overreach`" + `, write a handoff note (what you
completed + suggested smaller sub-tasks), commit your partial progress, and end. Do
NOT split the task yourself — a fresh, clean-context agent splits it far better than
an exhausted one. Judging "this is too big" is more reliable than guessing your own
token count, so trust the scope signal and hand off.

If instead you notice mid-work — while still well clear of that limit — that the task
is really two separate checkable changes, you may write the extra task file(s) into
` + "`specs/tasks/`" + ` and carry on with the current one. That is a normal in-loop split,
not an overreach.
`

// WriteDefault writes DefaultMarkdown to path, creating its parent directory. It
// NEVER overwrites an existing file — a user's tuned rules are theirs to keep, and
// `flanders init` is for the missing-file case — returning wrote=false (no error)
// when one is already present.
//
// The write is atomic (temp file in the same directory + rename), matching the
// discipline used for config.toml, state.json, and task files: a crash mid-write
// must never leave a half-written rules file that silently truncates the agent's
// contract on the next loop.
func WriteDefault(path string) (wrote bool, err error) {
	if path == "" {
		return false, errors.New("rules: WriteDefault needs a path")
	}
	switch _, statErr := os.Stat(path); {
	case statErr == nil:
		return false, nil // already present; do not clobber
	case !errors.Is(statErr, os.ErrNotExist):
		return false, fmt.Errorf("rules: stat %q: %w", path, statErr)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("rules: create %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".rules-*.tmp")
	if err != nil {
		return false, fmt.Errorf("rules: create temp in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.WriteString(DefaultMarkdown); err != nil {
		tmp.Close()
		return false, fmt.Errorf("rules: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("rules: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, fmt.Errorf("rules: rename temp to %q: %w", path, err)
	}
	return true, nil
}
