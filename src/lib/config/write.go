package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultTOML is the commented configuration file that `flanders init` writes
// when a project has no .flanders/config.toml yet (spec 03-config.md §"missing →
// flanders init writes a commented default").
//
// Why a hand-authored template and not an encoder dump: the BurntSushi TOML
// encoder cannot emit comments, and the value of an init file IS its comments —
// they teach the schema in place. So the text lives here as the single canonical
// "default file," and a test (TestDefaultTOMLMatchesDefault) parses it back and
// asserts it equals Default() (plus the [commands] starters), so the template can
// never silently drift from the documented defaults.
//
// Note the [commands] values are deliberately STARTERS for a Go project, not
// overlay-defaults: Default() leaves them empty (test has no default by design;
// an omitted build means "skip the compile check"). init writes "go test"/"go
// build" because a generated file should be runnable out of the box for the
// common case; edit them for your stack (auto-detection is OPEN in spec 03).
const DefaultTOML = `# Flanders configuration — .flanders/config.toml (see specs/03-config.md).
#
# Written by ` + "`flanders init`" + `. The harness overlays this file on its built-in
# defaults, so every line below may be removed to fall back to the default. The
# [commands] entries are starters for a Go project — edit them for your stack.

# ── paths (relative to project root) ──────────────────────────────
[paths]
specs   = "specs"                  # user-authored specs (input)
tasks   = "specs/tasks"            # generated task files
journal = ".flanders/journal"      # per-iteration logs
plan    = "IMPLEMENTATION_PLAN.md" # derived, human-readable checklist
state   = ".flanders/state.json"

# ── project commands (the harness's ground truth) ─────────────────
[commands]
test  = "go test ./..."  # REQUIRED for build; exit 0 = done-gate
build = "go build ./..." # optional pre-test compile check ("" disables)
lint  = ""               # optional; "" disables
# OPEN: auto-detect (go/npm/cargo…) to pre-fill during ` + "`init`/`discuss`" + `.

# ── how the agent is invoked ──────────────────────────────────────
[agent]
bin             = "claude"              # path/name of the CLI
permission_mode = "bypassPermissions"  # LOCKED default: --dangerously-skip-permissions
rules_file      = ".flanders/rules.md" # appended via --append-system-prompt
stream_input    = true                 # use --input-format stream-json (enables soft wind-down)

# ── model & effort, per agent class ───────────────────────────────
# See 07-agents-and-models.md. effort: low|medium|high|xhigh|max
[phases.discuss] # interactive spec-authoring (05-discuss.md)
model  = "opus"
effort = "high"

[phases.plan]
model  = "opus"
effort = "high"

[phases.build]
model  = "opus"
effort = "high"

[phases.test] # TDD: writes/verifies the acceptance test
model  = "sonnet"
effort = "medium"
# ` + "`split`" + ` is not its own class — it reuses [phases.plan].

[subagents] # default for agents a lead agent spins up
model  = "sonnet"
effort = "low" # cheap exploration; stretches the usage window
# optional per-class overrides, e.g.:
# [subagents.explore]
# model  = "haiku"
# effort = "low"

# ── context-pressure thresholds ───────────────────────────────────
[context]
window_tokens = 200000 # model's window; used to compute % (auto-detect by model if 0)
soft_pct      = 0.75   # inject graceful wind-down (mark blocked: context-overreach)
hard_pct      = 0.90   # hard-kill backstop; harness writes the block marker

# ── loop guardrails ───────────────────────────────────────────────
[guardrails]
max_iterations    = 100   # per phase; halt + surface when hit
stall_n           = 3     # consecutive no-change loops → halt
iteration_timeout = "20m" # per-loop wall-clock cap

# ── subscription usage handling (not a dollar budget) ─────────────
[usage]
on_limit   = "wait" # wait | halt
max_cycles = 0      # 0 = unlimited usage windows drained unattended
backoff    = "30m"  # fallback wait when reset time isn't parseable

# ── git checkpointing ─────────────────────────────────────────────
[git]
enabled         = true
init_if_missing = true       # offer to ` + "`git init`" + ` if the target isn't a repo
commit_each     = "progress" # progress | iteration | off
message_tmpl    = "Flanders: {phase} #{iter} — {task} [{result}]"
`

// WriteDefault writes the commented default config (DefaultTOML) to path,
// creating its parent directory. It NEVER overwrites an existing file — a user's
// edited config is precious and `flanders init` is for the missing-config case —
// returning wrote=false (and no error) when one is already present.
//
// The write is atomic (temp file in the same directory + rename), matching the
// discipline used for state.json and task files: a crash mid-write must not leave
// a half-written config that fails to parse on the next start.
func WriteDefault(path string) (wrote bool, err error) {
	if path == "" {
		return false, errors.New("config: WriteDefault needs a path")
	}
	switch _, statErr := os.Stat(path); {
	case statErr == nil:
		return false, nil // already present; do not clobber
	case !errors.Is(statErr, os.ErrNotExist):
		return false, fmt.Errorf("config: stat %q: %w", path, statErr)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("config: create %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return false, fmt.Errorf("config: create temp in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.WriteString(DefaultTOML); err != nil {
		tmp.Close()
		return false, fmt.Errorf("config: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("config: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, fmt.Errorf("config: rename temp to %q: %w", path, err)
	}
	return true, nil
}
