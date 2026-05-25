# Flanders — Discuss

> Status: **draft for review**. Layout/keys are a proposal to react to.

**Keywords:** discuss · interactive · chat · spec-authoring · stream-json · discuss-agent · spec-conventions · granularity-authority · user-authority · handoff · launch · readiness

## Purpose

`flanders discuss` is the **interactive** mode where a human and an agent
converse to author and refine `specs/*.md`. It is the *only* non-Ralph mode —
a real back-and-forth, not a loop — and it is effectively **this conversation,
productized**: discuss requirements → ask clarifying questions → capture decided
specifications into the `specs/` files, marking what's still open.

`specs/` is the single source of truth that the `plan` loop later consumes, so
discuss's whole job is making that directory clear, decided, and complete enough
to plan from.

## Nature & streaming

- Interactive chat over `--input-format stream-json --output-format stream-json`
  (same bidirectional plumbing as the loop's soft wind-down in `01-ralph-loop.md`).
- Long-lived session (unlike the loops, this *does* keep context — it's a
  conversation). Standard context management applies; the durable artifact is the
  spec files, not the chat.

## The discuss agent

- **Model/effort:** `opus`/`high` by default (discussion quality matters most
  here). Configurable as `[phases.discuss]` in `03-config.md`.
- **Tools:** Read/Write/Edit scoped to `specs/`, plus the usual exploration tools
  (it may read the target codebase to ground the specs). Runs under the locked
  `bypassPermissions` (the human is present and sees every edit live).
- **Role (system prompt):** drive toward decisions — surface options and
  trade-offs, ask focused clarifying questions, and *write decisions to disk as
  they're made* rather than at the end.

## Spec-authoring conventions it follows

The agent maintains the house style we've used here, so the output is
plan-ready:

- numbered, single-concern files (`NN-topic.md`); a `00-overview.md` index;
- a **Status** line per file; explicit **`OPEN`** markers for undecided points;
- cross-references between specs (`see 01-ralph-loop.md`);
- captures *rationale* with decisions (the plan loop benefits from the "why").

## Create *and* refine

Discuss both greenfields new specs and **iterates on existing ones** — it reads
the current `specs/` first, so a later session can revisit and amend decisions
(exactly how this set was built up incrementally).

## User owns spec granularity (locked)

The **user dictates how granular the specs are** — the agent must not impose its
own level of detail. It proposes and asks ("break this down further, or is this
enough?") and follows the user's chosen resolution: it does not run ahead writing
exhaustive detail the user didn't ask for, nor flatten points the user wants
spelled out.

This is distinct from *task* granularity in the `plan` phase, which follows the
mechanical rule (smallest checkable change — `02-plan-and-tasks.md`). Spec detail
is the human's call and *informs* how `plan` later decomposes; task sizing is the
harness's rule.

## TUI — Chat view

```
┌─ Flanders · discuss · ⚠ PERMISSIONS BYPASSED ····················· opus/high ┐
├───────────────────────────────────────────────────┬──────────────────────────┤
│ CONVERSATION                                        │ SPECS                     │
│ you ▸ I want a plan loop that…                      │  00-overview.md           │
│                                                     │  01-ralph-loop.md         │
│ ◆ Good — should each loop resume or run fresh?      │  02-plan-and-tasks.md     │
│   A) fresh (Ralph)   B) resume   C) hybrid          │  03-config.md    · edited │
│                                                     │  04-tui.md                │
│ you ▸ fresh                                         │  05-discuss.md   · new    │
│                                                     │                           │
│ ◆ Locked. Writing specs/01-ralph-loop.md…           │                           │
│   ⏺ Edit specs/01-ralph-loop.md (+18 -2)            │                           │
├───────────────────────────────────────────────────┴──────────────────────────┤
│ › type your message…                                            [enter] send    │
├────────────────────────────────────────────────────────────────────────────┤
│ [tab]focus [↑↓]scroll [d]iff last write [p]lan→ [?]help [esc]exit               │
└────────────────────────────────────────────────────────────────────────────┘
```

- **CONVERSATION** — the chat; `you ▸` user, `◆` agent. Tool calls that touch
  specs render inline (`⏺ Edit …`).
- **SPECS** — live list of `specs/*.md` with `· new` / `· edited` markers so you
  see what the agent just changed.
- Shares the Bubble Tea infra and palette from `04-tui.md`.
- `d` peeks the diff of the last spec write; `p` hands off to the `plan` loop.

## User authority: discussion only (locked)

The user's **only** control point is **completing the discussion phase and
launching the run.** Everything from `plan` onward is **fully autonomous and
untouched by a human** — no per-cycle approval, no plan→build gate.

- **discuss → launch:** discuss is always human-initiated and human-ended; it
  *never* auto-runs `plan`. On exit it may *suggest* "specs look ready — run
  `flanders plan`" (`p` triggers it). Running the command is the human's
  authority over moving on.
- **plan → build → re-plan → build …:** once launched, the pipeline runs to
  completion with no approval at any cycle. It pauses only on a guardrail halt or
  a usage-window wait (`01`) — exceptional events, not routine approval.

Bare `flanders` starts at `plan` and flows straight through `build`
autonomously. `discuss` is its own explicit, human-driven command.

## OPEN

- Whether a lightweight "readiness check" (no blocking `OPEN`s left) is offered
  before handoff, or left entirely to the human.
- Whether `discuss` can be re-entered *during* a paused build to amend specs
  mid-run, then resume.
