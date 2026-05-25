# Flanders — Overview

> Status: **in design**. This directory is the source of truth for what we are
> building. Nothing is implemented yet. Sections marked `OPEN` are undecided.

**Keywords:** overview · index · keyword-index · spec-map · commands · discuss · plan · build · auth · subscription · usage-model · vision

## What it is

Flanders is a single Go binary that wraps the Claude Code CLI and drives a
**Ralph loop** — repeatedly invoking a fresh agent against durable on-disk state
until the work is provably complete (all planned tasks done, real tests green).
It presents a TUI for observing and steering the loops.

## Tech stack

- **Language:** Go (1.24+).
- **TUI:** Bubble Tea (charmbracelet) — see `04-tui.md` (OPEN).
- **Agent:** shells out to the `claude` CLI in headless mode (`-p`,
  `--output-format stream-json`). There is no native Go Agent SDK, so the CLI is
  the integration surface. Target CLI: 2.1.x.

## Auth & usage model (locked)

The harness targets a **Claude Pro/Max subscription**, not per-token API billing.
Implications that shape the whole design:

- **No dollar budget.** Per-token cost is not a constraint; cost figures are
  informational only (a throughput proxy), never a stop condition.
- **Usage windows are the real limit.** The constraint is the subscription's
  rate/usage limits (rolling ~5-hour window + weekly cap). On hitting a limit the
  harness **pauses and auto-resumes at reset** — unattended, since all state is on
  disk (see `01-ralph-loop.md` §Guardrails → usage-limit handling).
- **Token economy = throughput, not money.** Lean loops (selective context,
  subagent exploration, small tasks) mean more tasks land per usage window.

## Command surface

| Command | Behaviour |
|---|---|
| `flanders discuss` | Interactive, harness-hosted chat to author `specs/*.md`. |
| `flanders plan` | Ralph loop: turn `specs/*.md` into a task plan (`specs/tasks/*`). |
| `flanders build` | Ralph loop: execute the plan until tests pass and all tasks are done. |
| `flanders` (bare) | Orchestrate `plan → build → plan → build …` until the whole plan is complete and green. |

## Core decisions (locked)

1. **Fresh context every iteration (pure Ralph).** Each loop is a brand-new
   `claude -p` session that knows only what is on disk (`specs/`, the plan, the
   journal). No session carries over. State lives in files, never in the agent's
   memory. This avoids context rot and forces durable state.

2. **The harness owns ground truth via the real test command.** "Done" is
   decided by the harness running the project's canonical test/verify command
   (exit 0) — not by the agent's self-assessment. The agent's opinion is
   advisory. See `01-ralph-loop.md`.

3. **The harness hosts the planning phase** (and discussion). Spec authoring and
   planning happen inside Flanders, not in a separate tool.

4. **Plan = per-task files; granularity = smallest checkable change.** The plan
   is a set of task files (`specs/tasks/NNNN-slug.md`) with structured
   frontmatter and a markdown body. A task is the smallest change with a
   checkable acceptance criterion (≈ one test going green). The human-readable
   checklist is *derived* from these files, never hand-maintained. See
   `02-plan-and-tasks.md`.

## Spec index (all drafted)

- `00-overview.md` — this file.
- `01-ralph-loop.md` — loop engine, agent invocation, done-detection, guardrails.
- `02-plan-and-tasks.md` — task file format and plan lifecycle.
- `03-config.md` — configuration (paths, commands, agents, thresholds, usage, git).
- `04-tui.md` — TUI layout, panes, controls, color palette.
- `05-discuss.md` — interactive spec-authoring chat.
- `06-orchestration.md` — bare-command plan↔build orchestration.
- `07-agents-and-models.md` — agent classes, models/effort, TDD test agent.
- `08-stream-json-protocol.md` — the `claude` event-stream contract the
  TUI/journal/guardrails parse (draft; wire shapes to pin vs CLI 2.1.x).
- `09-state-and-resume.md` — `state.json` schema + crash/usage-wait resume
  semantics (draft).

## Keyword index — where to look

Every spec carries a `**Keywords:**` line near its top, so `grep -ri <term>
specs/` lands on the right file. Common topics → spec:

| Looking for… | Spec |
|---|---|
| the loop, iterations, fresh context, agent invocation, `claude` flags | `01-ralph-loop.md` |
| guardrails (stall, context-pressure, timeout, usage-limit waiting) | `01-ralph-loop.md` |
| done-detection, test gate, journal, git checkpoints | `01-ralph-loop.md` |
| tasks, task files, frontmatter (`status`/`deps`/`acceptance`), granularity | `02-plan-and-tasks.md` |
| block reasons (`context-overreach`/`new-scope`), derived checklist | `02-plan-and-tasks.md` |
| config file, `config.toml`, paths, commands, thresholds, permission-mode | `03-config.md` |
| TUI, dashboard, panes, color palette, keybindings, controls | `04-tui.md` |
| discuss phase, interactive chat, spec authoring, granularity authority | `05-discuss.md` |
| orchestration, plan↔build, drain/batch-replan, autonomy, bare command | `06-orchestration.md` |
| agents, models, effort, subagents, TDD test agent, test→build→verify | `07-agents-and-models.md` |
| stream-json events, parser, live token tracking, usage-limit reset parse | `08-stream-json-protocol.md` |
| `state.json`, resume, crash recovery, run-state cache, ground-truth tiers | `09-state-and-resume.md` |
| auth/usage model (subscription), command surface, project vision | `00-overview.md` |

## Remaining open decisions (tracked)

Minor / deferrable — none block a first build:

- Git/checkpointing assumptions (the harness's own dir is not yet a git repo).
- Guardrail-halt recovery UX (`01`/`06`).
- A TUI judgment-call left to evolve in use (`04`): layout density.
- Test-command auto-detection; model→context-window table (`03`).
- How the `test` agent locates an existing test; `tdd=false` escape hatch (`07`).
- Optional task frontmatter fields / `attempts` counter (`02`).
