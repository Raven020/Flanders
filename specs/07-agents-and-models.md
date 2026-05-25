# Flanders — Agents & Models

> Status: **in design**. Locked items reflect decisions made; `OPEN` items remain.

**Keywords:** agents · models · effort · phase-agents · subagents · agent-classes · defaults · model-table · discuss-agent · test-agent · tdd · test-build-verify · visibility · agent-tree

## Two layers of agents

- **Phase (lead) agents** — drive a loop: `plan`, `build`, `test`. (`split`
  reuses the `plan` agent's settings — not its own class.)
- **Subagents** — spun up *inside* a loop by a lead agent (e.g. exploration/
  search agents that keep the main loop's context lean — see `01-ralph-loop.md`).

Each agent (both layers) has a **model** (alias `opus`/`sonnet`/`haiku` or full
id) and **effort/"power"** (`low`/`medium`/`high`/`xhigh`/`max`, via `--effort`).

## Configurability (locked)

Every agent **class** has a default, and the user can override the model and
effort of each class — phase agents in `[phases.*]`, subagents in `[subagents]`
(global default + optional per-class overrides). See `03-config.md`.

### Defaults

| Class | Model | Effort | Notes |
|---|---|---|---|
| `discuss` | opus | high | interactive spec authoring (`05-discuss.md`); not a loop |
| `plan` | opus | high | decomposition needs strong reasoning |
| `build` | opus | high | the implementer |
| `test` | sonnet | medium | lighter job (write/verify one test) |
| `split` | — | — | reuses `plan` settings |
| subagents (default) | sonnet | low | cheap exploration; stretches usage window |

These are starting points; the throughput lever (cheaper subagents / lower
effort = more tasks per usage window) is in the user's hands.

## The `test` agent (locked: TDD, always-on)

For each task the test agent's job is to **ensure a failing acceptance test
exists** — it always *checks*, but only *writes* when needed:

1. **Red test already exists** → reuse, write nothing → proceed to build.
2. **A test already passes** for the acceptance → report it; the harness **marks
   the task `done` and skips the build loop** (or flags if the test looks like it
   doesn't exercise the behavior).
3. **No test** → write a minimal red test encoding the acceptance criterion.

Critically, the test author is a **different agent than the implementer**, so the
build agent cannot weaken its own success criterion. This is what makes the
harness's ground-truth test gate trustworthy. (OPEN: a `tdd=false` escape hatch
if ever wanted — currently always-on.)

## Per-task flow within the build phase

```
for each actionable task:
  1. test loop   — test agent ensures a red acceptance test (or finds one)
                   ├ already green → mark done, skip build
                   └ red ready     → continue
  2. build loop(s)— build agent implements until the harness's test command passes
  3. verify       — harness runs the canonical test command (ground truth)
```

Each loop is still fresh-context and obeys all guardrails in `01-ralph-loop.md`
(context-overreach can occur in the build loop and triggers a fresh split as
usual). The `plan` phase is unchanged (decompose specs → tasks).

## Visibility (TUI requirement)

The TUI must show the **live agent tree**: the current phase/lead agent plus any
subagents it has spun up, each labelled with its model and effort, and live
status. Carry into `04-tui.md`.

## OPEN

- `tdd=false` escape hatch.
- Whether subagents need per-class overrides beyond the global default in v1.
- How the test agent locates "an existing test for this acceptance" (heuristic /
  naming convention / ask the test command to run a filtered subset).
