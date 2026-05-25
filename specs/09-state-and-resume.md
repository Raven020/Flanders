# Flanders — State & Resume

> Status: **draft for review**. Authored because `03-config.md` declares
> `paths.state = .flanders/state.json` and `04-tui.md` locks "closing/reopening
> also resumes," yet no spec defines what `state.json` holds or how recovery
> works. Field set below is a proposal; defaults/shape are `OPEN`.

**Keywords:** state · state.json · persistence · resume · recovery · crash · run-state
· cursor · ground-truth · checkpoint · usage-window · waiting · unattended · journal

## State hierarchy (the key decision)

Flanders is Ralph: **all durable truth lives in files + git**, never in process
memory. `state.json` is therefore a **resumable cursor and run-state cache**, not
an authoritative store. Three tiers, in order of authority:

1. **Ground truth** — `specs/tasks/*.md` (task statuses) and the **git history**
   (what was actually done). The done-gate is the real **test command**
   (`01-ralph-loop.md`). These can rebuild everything.
2. **Journal** — `.flanders/journal/` per-iteration records (append-only history).
3. **`state.json`** — a derived snapshot so a restart resumes instantly without
   re-deriving, and so the TUI can repaint the run state. If it is lost or stale,
   the harness **reconstructs** it from tiers 1–2 (it is a cache, not a master).

This hierarchy is what makes unattended multi-day runs and "close/reopen resumes"
safe (`04-tui.md` WAITING state, `00-overview.md` usage model).

## Proposed `state.json`

```json
{
  "schema_version": 1,
  "phase": "build",                  // discuss | plan | build | orchestrate
  "run_state": "WAITING",            // RUNNING | PAUSED | WAITING | HALTED | DONE
  "started_at": "2026-05-25T14:00:00Z",
  "updated_at": "2026-05-25T15:12:00Z",

  "iter": { "plan": 3, "build": 12, "total": 15 },  // per-phase + total (vs guardrail max)
  "current_task": "0007",            // task id the active/last loop targeted

  "stall": { "count": 0, "n": 3 },   // consecutive no-change loops / configured limit

  "usage": {                          // subscription window handling (00/01)
    "waiting": true,
    "reset_at": "2026-05-25T18:24:00Z",   // parsed reset; null → use backoff
    "cycles_used": 1                       // vs [usage].max_cycles
  },

  "halt": { "reason": "", "task": "" },     // populated only when run_state=HALTED
  "last_checkpoint": "3cb3837",             // git sha of the last harness commit
  "last_session_id": "…"                    // last claude session (journal cross-ref)
}
```

## Write discipline

- The harness writes `state.json` **atomically** (write temp + rename) at every
  state transition: phase change, loop start/end, status change, checkpoint,
  guardrail trip, usage wait/resume. Never partially written.
- Only the **harness** writes it. The agent never touches it (consistent with
  "do not hand-edit harness-owned state," `01-ralph-loop.md`).

## Resume / recovery semantics

On startup, if `state.json` exists:

- `run_state: WAITING` and `usage.reset_at` in the future → re-enter the
  usage-wait with a live countdown to `reset_at`; auto-resume at reset
  (`01-ralph-loop.md`). Past `reset_at` → resume immediately.
- `run_state: RUNNING` (i.e. a crash mid-loop — there was no clean exit) →
  **reconcile against ground truth before continuing**: re-read task files and
  `git status`/`git diff` to determine whether the interrupted loop actually
  landed work (mirrors the status-inference fallback in `02-plan-and-tasks.md`),
  set the task's status accordingly, then re-select the next task. The
  half-finished loop is *not* trusted; the harness re-derives.
- `run_state: HALTED` → restore the halt and its `reason` for the TUI recovery UX
  (`06-orchestration.md`, OPEN).
- `run_state: PAUSED` / `DONE` → restore as-is.
- **Missing/corrupt** `state.json` → rebuild a fresh snapshot from task files +
  journal + git, default `run_state: RUNNING` for the resolved phase, and carry on
  (cache miss, not an error).

## Relationship to other artifacts

- **Task files** own `status`; `state.json` only *points at* the current task.
- **`IMPLEMENTATION_PLAN.md`** is the *derived human checklist*
  (`02-plan-and-tasks.md`) — independent of `state.json`; both are regenerated
  from task files.
- **Journal** entries reference `last_session_id` for drill-in (`04-tui.md`).

## OPEN

- Exact field set / whether `iter` and `stall` belong in `state.json` vs. being
  re-derived from the journal each start (cache vs. recompute trade-off).
- Schema migration policy when `schema_version` bumps.
- Concurrency guard: a lockfile to prevent two `flanders` processes driving the
  same project dir at once.
