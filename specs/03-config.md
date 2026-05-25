# Flanders — Configuration

> Status: **draft for review**. This is a proposal to react to; defaults are
> suggestions, not locked.

**Keywords:** config · config.toml · TOML · settings · paths · commands · test-command · build-command · lint · permission-mode · bypassPermissions · model · effort · phases · subagents · context-window · thresholds · guardrails · usage · max-cycles · git · checkpoint

## Format & location

- **TOML** (locked), at `.flanders/config.toml` (per-project). Clean for
  sectioned config, idiomatic in Go, less whitespace-fragile than YAML. (Task
  files stay YAML-frontmatter; the two formats coexist by design.)
- The harness reads it at startup; missing → `flanders init` writes a
  commented default. Build phase **requires** `[commands].test`; everything else
  has a default.

## Proposed file

```toml
# ── paths (relative to project root) ───────────────────────────────
[paths]
specs   = "specs"                 # user-authored specs (input)
tasks   = "specs/tasks"           # generated task files
journal = ".flanders/journal" # per-iteration logs
plan    = "IMPLEMENTATION_PLAN.md"# derived, human-readable checklist
state   = ".flanders/state.json"

# ── project commands (the harness's ground truth) ─────────────────
[commands]
test  = "go test ./..."   # REQUIRED for build; exit 0 = done-gate
build = "go build ./..."  # optional pre-test compile check
lint  = ""                # optional; "" disables
# OPEN: auto-detect (go/npm/cargo…) to pre-fill during `init`/`discuss`.

# ── how the agent is invoked ──────────────────────────────────────
[agent]
bin             = "claude"        # path/name of the CLI
permission_mode = "bypassPermissions"  # LOCKED default: --dangerously-skip-permissions
rules_file      = ".flanders/rules.md"  # appended via --append-system-prompt
stream_input    = true            # use --input-format stream-json (enables soft wind-down)

# ── model & effort, per agent class ───────────────────────────────
# See 07-agents-and-models.md. effort: low|medium|high|xhigh|max
[phases.discuss]                  # interactive spec-authoring (05-discuss.md)
model  = "opus"
effort = "high"

[phases.plan]
model  = "opus"
effort = "high"

[phases.build]
model  = "opus"
effort = "high"

[phases.test]                     # TDD: writes/verifies the acceptance test
model  = "sonnet"
effort = "medium"
# `split` is not its own class — it reuses [phases.plan].

[subagents]                       # default for agents a lead agent spins up
model  = "sonnet"
effort = "low"                    # cheap exploration; stretches the usage window
# optional per-class overrides, e.g.:
# [subagents.explore]
# model  = "haiku"
# effort = "low"

# ── context-pressure thresholds ───────────────────────────────────
[context]
window_tokens = 200000   # model's window; used to compute % (auto-detect by model if 0)
soft_pct      = 0.75     # inject graceful wind-down (mark blocked: context-overreach)
hard_pct      = 0.90     # hard-kill backstop; harness writes the block marker

# ── loop guardrails ───────────────────────────────────────────────
[guardrails]
max_iterations    = 100  # per phase; halt + surface when hit
stall_n           = 3    # consecutive no-change loops → halt
iteration_timeout = "20m"# per-loop wall-clock cap

# ── subscription usage handling (not a dollar budget) ─────────────
[usage]
on_limit       = "wait"  # wait | halt
max_cycles     = 0       # 0 = unlimited usage windows drained unattended
backoff        = "30m"   # fallback wait when reset time isn't parseable

# ── git checkpointing ─────────────────────────────────────────────
[git]
enabled        = true
init_if_missing= true     # offer to `git init` (harness's own dir isn't a repo yet)
commit_each    = "progress" # progress | iteration | off
message_tmpl   = "Flanders: {phase} #{iter} — {task} [{result}]"
```

> No plan-approval knob: once launched the pipeline is fully autonomous (the only
> human control point is completing discussion — see `05`/`06`).

## Notes on the non-obvious fields

- **`permission_mode = bypassPermissions` (LOCKED).** Loops run with
  `--dangerously-skip-permissions` so headless `-p` never deadlocks on an
  unanswerable prompt. This is high-trust: the agent can run any tool/command.
  **Requirement:** the TUI must display a **persistent, always-visible warning**
  that permission checks are bypassed (carry into `04-tui.md`). Intended to be
  run in a sandbox/VM. (No `allowed_tools` needed under bypass.)
- **`stream_input`.** `true` enables the soft wind-down at `soft_pct` (the harness
  injects a "wrap up" message via `--input-format stream-json`). If `false`, we
  skip straight to the `hard_pct` backstop — simpler, but no graceful handoff note.
- **`context.window_tokens`.** Needed to turn token counts into a %. `0` →
  harness picks the known window for the configured model. (OPEN: maintaining a
  model→window table vs. requiring the number.)
- **`usage.on_limit = wait`** is the behavior you asked for; `halt` is for anyone
  who'd rather stop than sleep.
- **`commit_each = progress`** commits only when a loop made real progress
  (status change or passing tests), matching the checkpoint rule in `01`.

## OPEN

- Test-command auto-detection during `init`/`discuss`.
- Model→context-window table vs. explicit `window_tokens`.
- Whether per-phase model/effort belongs here or can be overridden per-run via flags.
- Per-class subagent overrides beyond the global `[subagents]` default — see
  `07-agents-and-models.md`.
