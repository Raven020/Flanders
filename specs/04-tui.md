# Flanders — TUI

> Status: **draft for review**. Layout and keys are a proposal to react to.

**Keywords:** tui · bubbletea · lipgloss · dashboard · panes · plan-pane · live-stream · agent-tree · meters · color-palette · theme · keybindings · controls · pause · stop · intervene · journal-view · task-detail · permissions-bypassed-warning · waiting-state · no-tui · headless

## Framework

Bubble Tea (charmbracelet) + Lipgloss for styling, Bubbles components (viewport
for scrollable streams, list for tasks). The harness emits parsed stream-json
events and state changes on a channel; the TUI consumes them as Bubble Tea
messages (Elm-style model→update→view). Handles terminal resize.

## Color palette (base — expected to evolve)

Truecolor via Lipgloss. Base palette and semantic roles:

| Hex | RGB | Role |
|---|---|---|
| `#d121db` | 209,33,219 — magenta | Brand / header app name · current-task marker `◀` · subagent spawns (`Task→`) |
| `#3dcb73` | 61,203,115 — green | Success: `[x]` done · tests passing · `RUNNING`/`DONE` · `● running`/`✓` · low ctx |
| `#d9132a` | 217,19,42 — red | Danger: **`⚠ PERMISSIONS BYPASSED`** · `[!]` blocked · errors `✗` · `HALTED` · ctx ≥ hard(90) |
| `#e18522` | 225,133,34 — orange | Caution: `[~]` active · `PAUSED`/`WAITING` · stall rising · ctx ≥ soft(75) |
| `#0897cd` | 8,151,205 — blue | Info / neutral: pane borders & titles · tool calls `⏺` · meters · cost (info-only) |

Notes:
- The context-% bar transitions **green → orange (≥75 soft) → red (≥90 hard)**,
  reusing the success/caution/danger colors so the trip marks read at a glance.
- The `PERMISSIONS BYPASSED` warning is red and high-contrast (bold / inverse),
  never dimmed — it must stay loud (`03-config.md`).
- The palette is the starting theme; colors are configurable (`[tui].theme` /
  per-role overrides — exact keys TBD) since the UI will evolve in use.

## Default view — Dashboard

```
┌─ Flanders · build · ⚠ PERMISSIONS BYPASSED ··········· iter 12/100 · RUNNING ┐
├───────────────────────────────┬──────────────────────────────────────────────┤
│ PLAN  (8/14 done)             │ LIVE  · 0007 loop engine · build(opus/high)    │
│ ▸ Phase 1: core               │                                                │
│   [x] 0001 parse stream-json  │  ⏺ Read engine/loop.go                          │
│   [x] 0005 task selector      │  ⏺ Edit engine/loop.go (+42 -3)                 │
│   [~] 0007 loop engine    ◀   │  ⏺ Bash: go build ./...   ✓                     │
│   [ ] 0008 checkpoint git     │  💬 "wiring stall detection into the tick…"      │
│ ▸ Phase 2: tui                │  ⏺ Task→ explore "where are guardrails?"        │
│   [!] 0009 blocked: ctx-over  │                                                │
│   [ ] 0010 dashboard render   │                                                │
├───────────────────────────────┴──────────────────────────────────────────────┤
│ AGENTS  build (opus/high) ─┬─ explore (sonnet/low) ✓ done                       │
│                            └─ explore (sonnet/low) ● running                    │
├────────────────────────────────────────────────────────────────────────────┤
│ ctx 62% ▓▓▓▓▓▓░░░░ (soft 75/hard 90)  ·  stall 0/3  ·  usage resets 3h12m  ·  $1.20 info │
├────────────────────────────────────────────────────────────────────────────┤
│ [p]ause [s]top [i]ntervene [j]ournal [tab]focus [↑↓]scroll [?]help               │
└────────────────────────────────────────────────────────────────────────────┘
```

### Header bar (always visible)
- app · current phase · **`⚠ PERMISSIONS BYPASSED`** (persistent, high-contrast —
  required by `03-config.md`) · iteration `n/max` · run state.
- **Run states:** `RUNNING` · `PAUSED` · `WAITING` (usage reset) · `HALTED`
  (guardrail trip) · `DONE`.

### Panes
1. **PLAN** (left) — the derived checklist (`02-plan-and-tasks.md`), live status
   markers: `[ ]` pending · `[~]` active · `[x]` done · `[!]` blocked (+reason).
   `◀` marks the current task. Grouped by phase. Selectable → Task detail.
2. **LIVE** (right, primary) — the current loop's activity, rendered from
   stream-json (not raw): `⏺` tool calls (name + terse args / result ✓✗), `💬`
   assistant text, `Task→` subagent spawns. Auto-scrolls; scrollback on focus.
3. **AGENTS** — live agent tree: lead phase agent + spun-up subagents, each
   `name (model/effort)` with status (`● running` / `✓ done` / `✗ error`).
   Satisfies the visibility requirement from `07-agents-and-models.md`.
4. **METERS** — context-% bar with the 75/90 trip marks, stall counter `k/N`,
   usage-window countdown, cost (info-only label, never a limit).

## Other views (toggle)
- **Journal** (`j`) — scrollable history of past iterations from
  `.flanders/journal/`: per-loop task, result, files touched, duration,
  tokens. Drill in for the full stream-json of any past loop.
- **Task detail** (`enter` on a task) — frontmatter (status/reason/deps/
  acceptance) + body + that task's loop history.
- **Help** (`?`).

## Controls
| Key | Action |
|---|---|
| `p` | Pause / resume — pause takes effect *after the current loop completes* (no mid-loop kill = no lost work). |
| `s` | Stop — graceful (finish current loop, checkpoint, exit); `S`/confirm for hard stop. |
| `i` | Intervene — write a note for the **next** loop (persisted to an operator-notes file the harness folds into that loop's prompt). **No live-steering of the running loop**: that would inject off-disk context and break Ralph's fresh-context / all-state-on-disk invariant (`01`). |
| `j` | Journal view. |
| `tab` | Cycle pane focus; `↑↓`/`PgUp/PgDn` scroll focused pane. |
| `enter` | Open selected task detail. |
| `?` | Help. `q` quit (graceful stop first). |

## WAITING (usage) state
When paused on a usage limit (`01-ralph-loop.md`), header shows `WAITING`, the
meter shows the live countdown to reset, and the TUI stays open; it auto-resumes
at reset (state is on disk, so closing/reopening also resumes).

## Headless / non-TTY
For truly unattended multi-day runs, a `--no-tui` (or auto when stdout isn't a
TTY) mode prints structured progress lines instead of the dashboard, same
underlying event stream. (OPEN: exact log format.)

## OPEN
- Single-screen dashboard vs. making LIVE full-screen by default with panes on toggle.
- `--no-tui` log format.
- Theme config keys (`[tui].theme` + per-role overrides). Base palette locked
  above; the UI is expected to evolve through use.
