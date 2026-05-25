# Flanders — The Ralph Loop

> Status: **in design**. Locked items reflect decisions made; `OPEN` items remain.

**Keywords:** ralph · loop · iteration · fresh-context · agent-invocation · claude-flags · stream-json · prompt-composition · selective-context · subagents · done-detection · test-gate · guardrails · max-iterations · stall · context-pressure · soft-wind-down · hard-kill · usage-limit · auto-resume · journal · git-checkpoint

## Principle

Fresh context every iteration; durable state on disk. Each loop is a clean
`claude -p` invocation that reads the current state, performs **one unit of
work**, writes results back to disk, and exits. The harness — not the agent —
owns the loop, the stopping logic, and ground truth.

## Iteration anatomy

```
loop:
  1. select  — harness picks the next actionable task from the plan
  2. compose — build the prompt: task file + dependency outcomes
               + relevant spec excerpts + compact done/left summary + rules
  3. spawn   — claude -p … --output-format stream-json (fresh session)
  4. observe — parse the event stream → TUI panes + journal entry
  5. verify  — harness runs the canonical test command (ground truth)
  6. evaluate— update task status; check guardrails; decide loop/stop
  7. checkpoint — git commit on progress
  8. repeat
```

## Agent invocation

Each loop spawns a fresh session. Baseline flags:

- `-p` — non-interactive single turn.
- `--output-format stream-json --verbose --include-partial-messages` — parseable
  event stream feeding the TUI and journal.
- `--session-id <new-uuid>` — fresh context per loop (no `--resume`/`--continue`).
- `--dangerously-skip-permissions` *or* `--permission-mode acceptEdits` —
  autonomous operation. (OPEN: which default; likely configurable.)
- `--max-budget-usd <n>` — API-key mode only; **off by default** (subscription
  usage has no per-token dollar cost — see `00-overview.md`).
- `--model` / `--effort` — per-phase (plan vs build) tuning, from config.
- `--append-system-prompt` — inject loop rules (one task per loop, update status,
  do not hand-edit harness-owned state, etc.).

The prompt body itself is composed by the harness (see "Prompt composition").

## Prompt composition (the cost/quality lever)

With fresh context, the dominant lever for both cost and quality is injecting
**only what the current task needs**, never the whole plan or journal:

- the current task file (description + acceptance criteria + deps),
- outcomes/summaries of the tasks it depends on,
- only the named spec excerpts the task references,
- a one-line "done / remaining" summary of the overall plan,
- the loop rules (via `--append-system-prompt`).

Focused context = fewer tokens *and* sharper work.

A key loop rule: **delegate exploration/search to subagents.** A subagent's
context does not count against the main loop's window — it returns only the
conclusion. This is the primary lever for keeping a single task's context lean
and under the context-pressure threshold (see Guardrails).

## Done-detection (locked: harness-owned)

The loop/phase is **done** iff all of:

1. the canonical **test command exits 0** (ground truth, run by the harness), and
2. **every task is `done`** in the plan, and
3. **no stall** is in effect.

The agent may *report* completion, but that report is advisory and never the
stop condition.

## Guardrails

- **Max iterations** — hard cap per phase; halt + surface when hit.
- **Usage-limit handling** (not a dollar budget — see `00-overview.md`) — the
  subscription's rate/usage windows are the real ceiling. On a rate-limit /
  usage-exhausted signal (error in the `result` event / non-zero exit), the
  harness parses the reported **reset time**, **pauses the loop, sleeps until
  reset, and auto-resumes** — unattended, since all state is on disk. The TUI
  shows a countdown. Fallbacks/knobs: if reset time isn't parseable, use a
  configured backoff; an optional `max_cycles` caps how many usage windows the
  harness will drain unattended (default: unlimited). Cost figures from
  `total_cost_usd` are tracked for *info/throughput* only, never as a stop.
- **Stall detection** — if N consecutive loops produce no file changes AND no
  task-status change, halt and surface (Ralph's classic failure mode). N is
  configurable.
- **Per-iteration timeout** — kill and record a loop that exceeds a wall-clock cap.
- **Context-pressure trip** — the harness tracks token usage live from the
  stream-json events. Past ~80% of the window quality degrades and
  auto-compaction risks mangling the task, so a single loop must never run deep.
  This is treated as a *granularity signal*: the task was too big for one
  fresh-context pass. The loop is ended and the task is marked
  `blocked: context-overreach`; the **split is performed fresh** by a
  clean-context agent (never by the exhausted loop — see `06-orchestration.md`).
  The marker is guaranteed three ways, in order of preference:
  1. **Proactive (agent judgment).** Loop rule: if the agent realizes the task
     is bigger than one focused pass, it stops early — sets `status: blocked`,
     `reason: context-overreach`, writes a handoff note (progress + suggested
     sub-tasks), commits, and ends. (Scope judgment is more reliable than the
     agent introspecting its own token count.)
  2. **Soft wind-down (~75%, harness-steered).** The harness injects a "wrap up
     now: mark blocked context-overreach, write handoff, commit, end" message
     into the running session via `--input-format stream-json`, so the agent
     winds down gracefully and leaves a handoff note.
  3. **Hard backstop (~90%, or non-compliance).** The harness kills the process
     and — because it knows why it killed — **writes `blocked: context-overreach`
     to the task file itself**, plus a git-diff summary of partial progress. The
     signal is never lost.

  All thresholds configurable.

Any guardrail trip pauses the loop and raises it in the TUI rather than failing
silently. (OPEN: exact recovery UX — retry / skip task / edit / abort.)

## Checkpointing & journal

- **Checkpoint:** on progress (status change or passing tests), the harness makes
  a git commit so iterations are revertable. Assumes the working *project* is a
  git repo; if not, the harness offers to `git init`. (NOTE: the harness's own
  directory is not yet a git repo.)
- **Journal:** each loop writes a record under `.flanders/journal/` —
  the raw stream-json plus a short summary (task, files touched, test result,
  cost, tokens, duration). The journal is the only "memory" across the fresh
  contexts, and what the TUI's history pane renders.

## OPEN

- Default permission mode for autonomous loops.
- Guardrail recovery UX.
- Whether the agent flips `status:` directly or emits a structured verdict the
  harness reconciles (see `02-plan-and-tasks.md`).
- How the harness discovers the canonical test command (config — `03-config.md`).
