# Flanders — Orchestration

> Status: **in design**. Locked items reflect decisions made; `OPEN` items remain.

**Keywords:** orchestration · phases · discuss-plan-build · bare-command · plan-completeness · drain · batch-replan · autonomy · approval-free · refinement · context-overreach · new-scope · tdd-flow · test-build-verify · termination

## Phases

`discuss → plan → build`, coordinated by the bare `flanders` command. Each
phase is a Ralph loop (except `discuss`, which is interactive). All loops obey
fresh-context + harness-owned ground truth (see `01-ralph-loop.md`).

Within the build phase, each task runs **test → build → verify** (TDD,
always-on): a `test` agent ensures a failing acceptance test exists (or finds the
task already satisfied and skips build), then the `build` agent makes it pass.
See `07-agents-and-models.md` for the per-task flow and agent classes.

## Plan-completeness criterion (locked)

The plan phase is "complete enough" to start building when **every requirement
in `specs/*.md` maps to at least one task** in `specs/tasks/*.md`. We do not try
to prove the plan is perfect up front — residual gaps are expected and surface
later during build as `blocked` tasks, then get batch-corrected (below). This
keeps the plan phase from looping forever chasing completeness.

## Build: drain, then batch re-plan (locked)

Build never halts the whole cycle on a single gap. Specifically:

1. **No per-gap halt.** When a build loop hits something the plan didn't cover,
   the agent marks *that task* `blocked` (with a reason) and the harness moves on
   to the next unblocked task.
2. **Drain.** Build keeps going until all buildable work is exhausted — every
   task is `done` or `blocked`.
3. **Batch re-plan.** Only then, if `blocked` tasks exist, the orchestrator runs
   **one focused plan loop** that resolves just those blocks (create/split the
   missing tasks), then resumes build.

Result: at most one phase transition per "build ran out of buildable work"
boundary — never one per snag. This minimizes the number of phase switches,
which is the real cost (each switch is an extra fresh-context spin-up), since
fresh-context means no live context is lost in a switch anyway.

## Refinement vs. new scope (locked)

The line that keeps most snags from ever bouncing to plan:

- **In-loop split (agent has context to spare)** — the agent notices mid-work
  that a task is really two tests, while well clear of the context threshold. It
  writes the extra task file(s) and carries on. No phase switch.
- **Context-overreach** — the task is too big to finish in one pass (crossed the
  context-pressure threshold). The loop is ended and the task marked
  `blocked: context-overreach` (see `01-ralph-loop.md` for how the marker is
  guaranteed). The exhausted loop **never splits itself** — an agent out of
  context is the worst at decomposition. The split is done **fresh** by a
  clean-context agent: either folded into the batched re-plan, or via a
  lightweight dedicated *split pass* (a tiny fresh agent whose only job is
  "given this task + handoff note, emit 2–4 smaller task files"). Guaranteed
  cheaply resolvable — it only needs decomposition.
- **New scope** — a genuinely missing requirement. Mark `blocked` (with reason);
  defer to the batched re-plan, which may require real planning, not just a split.

## Autonomy (locked)

Once launched, the `plan → build` pipeline is **fully autonomous** — no human
approval at any cycle, no plan→build gate. The **only** human authority is
completing discussion and launching the run (`05-discuss.md`). The pipeline
pauses only on a guardrail halt or a usage-window wait (`01`) — exceptional
events, not routine approvals.

## Bare `flanders` flow

```
plan loop      → until plan-complete (every spec req → ≥1 task)
build loop     → until all tasks done | blocked   (drain)        ← autonomous
if blocked:    → focused plan loop on blocks → resume build   (repeat)
done when:     test command exits 0
               AND all tasks `done` (none pending/blocked)
               AND guardrails not tripped
```

## Termination & handoff

- **Success:** the overall done-condition above holds. The harness reports a
  summary (tasks, cost, iterations, duration).
- **Halt:** any guardrail trip (`01-ralph-loop.md`) pauses and surfaces in the
  TUI for the user to retry / skip / edit / abort. (OPEN: exact recovery UX.)

## OPEN

- Recovery UX on guardrail halt.
- Loop budget split across phases (how cost/iter caps are apportioned).
