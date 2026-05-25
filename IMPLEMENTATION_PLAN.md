# Flanders — Implementation Plan

> **Status:** Phase 0 (project foundation) COMPLETE; Phase 1 in progress — 1.1
> (Config loader) and 1.3 (Task-file model) COMPLETE. Go module (`module flanders`,
> go 1.24), layout (`src/cmd/flanders` + `src/lib/{paths,logging,config,task}`),
> file-backed slog logger, paths helper, config loader, and the task-file model all
> exist with passing tests. `go build ./...`, `go vet ./...`, and `go test ./...`
> are all green. **Next up: 1.4 Task store / selector** (now unblocked — it
> consumes `src/lib/task`), then 1.2/1.5/1.6.
>
> **Goal:** build **Flanders** — a single Go (1.24+) binary that wraps the
> `claude` CLI and drives a Ralph loop, per `specs/00`–`09`.
>
> **Priority order:** top-to-bottom = build order. Lower phases depend on higher
> ones. Within a phase, items are roughly dependency-sorted. Items are sized
> toward the spec's "smallest checkable change" rule (`02-plan-and-tasks.md`) so
> each maps to ≈ one test going green.

---

## ⚠ Read first — two meta-notes

- **Bootstrap plan vs. derived checklist.** `02-plan-and-tasks.md` says
  `IMPLEMENTATION_PLAN.md` is *generated from `specs/tasks/*.md` and never
  hand-maintained*. That rule describes Flanders operating on a **target
  project**. Right now we are **hand-building Flanders itself** and no task files
  exist yet, so **this file is the hand-maintained bootstrap plan for building the
  harness.** Once the plan loop (Phase 4) and the derived-checklist generator
  (Phase 6) exist, Flanders will generate the checklist for *its* targets; this
  bootstrap file can then be retired or kept purely as dev notes. Do not confuse
  the two.
- **`src/lib` is the standard library.** Put shared primitives (config, task-file
  model, stream-json parser, paths, journal, git, logging) in `src/lib` and have
  every consumer import them. No ad-hoc copies. (Prompt directive.)

---

## Phase 0 — Project foundation  `[blocks everything]`

- [x] **0.1 Go module + layout.** `go.mod` (module path, Go 1.24+). Decide layout:
  `src/cmd/flanders` (main), `src/lib/*` (stdlib), feature packages under `src/`.
  *Acceptance:* `go build ./...` succeeds on an empty skeleton.
  (go.mod `module flanders`/go 1.24; layout src/cmd/flanders + src/lib/*; `go build ./...` green)
- [x] **0.2 Toolchain confirmed.** Verify `go` is installed/usable in the run
  environment; record exact build/test commands in `AGENTS.md`.
  (go 1.24.1 confirmed; commands recorded in AGENTS.md)
- [x] **0.3 Logging primitive** in `src/lib` (leveled, file-backed under
  `.flanders/`, non-interleaving with the TUI). Spec 01 §journal + "extra logging"
  (PROMPT rule). *Acceptance:* log lines written and rotated/segregated from TUI.
  (`src/lib/logging`: slog-based, file-backed to `.flanders/flanders.log`, segregated from TUI. NOTE: log ROTATION deferred — segregation satisfies the non-interleave requirement; add rotation later if log size becomes an issue.)
- [x] **0.4 Paths helper** in `src/lib`: resolve `[paths]` (specs, tasks, journal,
  plan, state) relative to project root; create `.flanders/` on demand.
  (`src/lib/paths`: New/EnsureFlanders/FindRoot; resolves specs/03 [paths] defaults + rules/config/log; creates `.flanders/` on demand)

## Phase 1 — Config & data model (`src/lib` core)  `[depends: 0]`

- [x] **1.1 Config loader.** Parse `.flanders/config.toml` → typed struct with the
  full schema in `03-config.md` (`[paths] [commands] [agent] [phases.*]
  [subagents] [context] [guardrails] [usage] [git]`). Apply documented defaults;
  **error if `[commands].test` missing for build**. *Acceptance:* loads the sample
  config and a minimal config (defaults fill in); missing test command rejected
  for build phase. (`03-config.md`)
  (Implemented in `src/lib/config` (package `config`): `Config` struct mirrors every `03-config.md` section; `Default()` returns all documented defaults; `Load(path)` overlays the file on top of `Default()` (absent keys keep defaults, present keys win); `Validate()` checks enums/ranges; `ValidateForBuild()` enforces the required `[commands].test`. TOML library decision RESOLVED: `github.com/BurntSushi/toml v1.4.0` (mature/stable; supports `encoding.TextUnmarshaler` for duration fields and a custom `UnmarshalTOML` for the mixed `[subagents]` section). `[commands].test` intentionally has NO default (a default would make "missing" undetectable); it is enforced by `ValidateForBuild`, not `Load`. Duration fields (`iteration_timeout`, `backoff`) parse into a `config.Duration` (wraps `time.Duration`). Per-class subagent overrides (`[subagents.<name>]`) are parsed into `Subagents.Classes` (forward-compat; OPEN for v1). All config tests pass; `go build/vet/test ./...` green.)
- [ ] **1.2 `flanders init`.** Write a commented default `config.toml` when absent.
  *(Note: `init` is referenced in `03-config.md` but absent from the command
  surface in `00-overview.md` — see Findings.)* *Acceptance:* `init` produces a
  loadable, commented config.
- [x] **1.3 Task-file model** in `src/lib`. Parse/serialize **YAML frontmatter +
  markdown body**: `id`, `status` (pending|active|done|blocked), `reason`
  (required iff blocked; taxonomy `context-overreach|new-scope|dependency|error`),
  `deps[]`, `acceptance`, optional `notes`/`files`/`attempts`. Round-trips without
  losing body or unknown fields. *Acceptance:* parse→serialize is lossless;
  blocked-without-reason rejected. (`02-plan-and-tasks.md`)
  (Implemented in `src/lib/task` (package `task`). KEY DESIGN: the frontmatter is
  held as a `gopkg.in/yaml.v3` `yaml.Node` (the single source of truth), NOT a
  plain struct — this is what makes the round-trip truly lossless: unknown keys,
  key order, AND inline comments all survive parse→serialize (a struct decode
  would drop all three). Typed accessors (`ID/Status/Reason/Deps/Acceptance/
  Notes/Files`) and setters (`SetStatus/SetBlocked/SetDeps`) are a thin view over
  the node, so there is no struct↔node drift. `id` and `deps` are read verbatim,
  so zero-padding like `0007`/`0001` is preserved (selector 1.4 must normalize ids
  when matching deps→ids). INVARIANT: `SetStatus` to any non-blocked status auto-
  clears `reason`, and `SetBlocked(reason)` is the only way to reach a blocked
  state — so "reason iff blocked" is hard to violate by construction, not just
  caught at `Validate`. `Validate()` requires id+acceptance+valid-status and
  enforces reason↔blocked. Frontmatter split: the closing `---` is the FIRST `---`
  line after the opener, so a markdown horizontal-rule `---` in the body is not
  mistaken for it; CRLF and a leading BOM are tolerated. `WriteFile` is atomic
  (temp-in-same-dir + rename). NEW DEP: `gopkg.in/yaml.v3 v3.0.1` (task files are
  YAML by design; config stays TOML). All task tests + full suite green.)
- [ ] **1.4 Task store / selector.** Enumerate `specs/tasks/*.md`; select the next
  actionable task = `pending` with **all `deps` `done`**; never select a task with
  unmet deps. Detect dependency cycles. *Acceptance:* selector returns correct
  next task across dep graphs; cycle surfaced as error. (`01` §select, `02` §deps)
- [ ] **1.5 State persistence** (`state.json`, `09-state-and-resume.md`). Atomic
  write (temp+rename) on every transition; load on startup; rebuild from task
  files+journal+git when missing/corrupt. *Acceptance:* round-trip; corrupt file
  recovers without crashing.
- [ ] **1.6 Journal writer** (`.flanders/journal/`, `01` §journal). Per-loop
  record: raw stream-json + summary (task, files touched, test result, cost,
  tokens, duration, session id). Append-only; readable back for the TUI history.
  *Acceptance:* a loop produces a re-readable journal entry.

## Phase 2 — Agent integration & stream-json  `[depends: 1; highest technical risk]`

- [ ] **2.1 Stream-json parser** in `src/lib` (`08-stream-json-protocol.md`).
  Streaming NDJSON decoder → typed events + a derived `LoopObservation` (tokens,
  cost, tool calls, subagent spawns, result/error, usage-limit + reset). Skip
  unparseable lines without crashing; preserve unknown types for the journal.
  *Acceptance:* fixture-based test over a captured real `claude 2.1.x` transcript
  asserts text/tool_use/result/token-usage extraction. **Capture a real transcript
  first** to pin wire shapes (spec 08 OPEN).
- [ ] **2.2 Live token / context-occupancy tracker.** Fold `message_start` /
  `message_delta` usage into a running % of `[context].window_tokens`; expose for
  meters + guardrail. *Acceptance:* synthetic stream drives the % monotonically;
  trips at soft/hard. (`08` §live token tracking, `01` §context-pressure)
- [ ] **2.3 Usage-limit detection + reset parse.** Classify an error `result` /
  non-zero exit as usage-limit vs. genuine error; extract `reset_at` (or fall back
  to `[usage].backoff`). *Acceptance:* known limit payloads → wait+reset; ordinary
  errors → error path. **Riskiest parse — verify wording vs 2.1.x.** (`08`, `01`)
- [ ] **2.4 CLI invocation builder.** Compose `claude` args from config/phase: `-p`,
  `--output-format stream-json --verbose --include-partial-messages`, fresh
  `--session-id <uuid>` (no resume/continue), permission mode
  (`--dangerously-skip-permissions`, LOCKED default), `--model`/`--effort`
  per-phase, `--append-system-prompt` (rules), `--input-format stream-json` when
  `stream_input`. **No `--max-budget-usd` by default** (subscription). *Acceptance:*
  builder emits expected argv per phase/config. (`01` §invocation, `03`)
- [ ] **2.5 Process supervisor.** Spawn/stream/wait the CLI; capture stdout(events)
  + stderr; enforce per-iteration timeout (kill); expose a writer for stream-json
  input injection (soft wind-down). *Acceptance:* runs a stub command, streams
  output, times out + kills cleanly.

## Phase 3 — The Ralph loop engine  `[depends: 1,2; core of the product]`

- [ ] **3.1 Iteration driver** implementing the 8-step anatomy
  select→compose→spawn→observe→verify→evaluate→checkpoint→repeat (`01`).
- [ ] **3.2 Prompt composition (cost/quality lever).** Inject only: current task
  file + dependency outcomes + named spec excerpts + one-line done/left summary;
  rules via `--append-system-prompt`. Never the whole plan/journal. *Acceptance:*
  composed prompt contains the task + referenced excerpts and excludes unrelated
  tasks. (`01` §prompt composition)
- [ ] **3.3 Loop rules file** (`.flanders/rules.md`): one task/loop, flip own
  `status`, don't hand-edit harness state, delegate exploration to subagents,
  proactive context-overreach handoff. (`01`, `02`, `03` `rules_file`)
- [ ] **3.4 Test gate (ground truth).** Run `[commands].test` (+ optional
  `build`/`lint`); exit 0 = pass. Harness-owned, not agent self-report.
  *Acceptance:* gate reflects real exit code. (`00` decision 2, `01` §done)
- [ ] **3.5 Status reconciliation / inference fallback.** After a loop, infer
  outcome from `git diff` (work happened?) + test gate; when the harness itself
  ends a loop, **write `status`/`reason` directly**. *Acceptance:* outcome recorded
  whether or not the agent flipped status. (`02` §mutation ownership)
- [ ] **3.6 Git checkpointing.** Commit on progress (status change or passing
  tests); `commit_each` modes; `message_tmpl`; offer `git init` if target isn't a
  repo. *Acceptance:* progress commit created with templated message. (`01`, `03`)
- [ ] **3.7 Done-detection.** Done iff test exits 0 **and** every task `done`
  **and** no stall. Agent report is advisory only. (`01` §done-detection)
- [ ] **3.8 Guardrail: max-iterations** per phase → halt + surface. (`01`,`03`)
- [ ] **3.9 Guardrail: stall** — N consecutive no-file-change *and* no-status-change
  loops → halt. *Acceptance:* halts after `stall_n`. (`01`,`03`)
- [ ] **3.10 Guardrail: per-iteration timeout** — kill + record. (uses 2.5)
- [ ] **3.11 Guardrail: context-pressure (three-tier).** (a) proactive agent
  handoff (rule-driven); (b) **soft wind-down ~75%** via injected stream-json
  "wrap up" message when `stream_input`; (c) **hard kill ~90%** where the harness
  writes `blocked: context-overreach` + git-diff summary itself. Marker guaranteed
  all three ways. **Exhausted loop never splits itself.** *Acceptance:* each tier
  leaves a `blocked: context-overreach` task + handoff. (`01` §context-pressure,
  `06` §refinement)
- [ ] **3.12 Guardrail: usage-limit wait/auto-resume.** On limit (2.3): set
  `WAITING`, persist `reset_at`, sleep to reset (or `backoff`), auto-resume;
  honor `[usage].on_limit` (wait|halt) and `max_cycles`. State on disk ⇒
  close/reopen resumes. *Acceptance:* simulated limit → wait → resume; `halt` mode
  stops. (`01`, `09`)

## Phase 4 — Phases & agent classes  `[depends: 3]`

- [ ] **4.1 Agent-class resolution.** Map phase/subagent → model+effort from
  `[phases.*]`/`[subagents]` (+ overrides); `split` reuses `plan`. *Acceptance:*
  each class resolves to documented defaults unless overridden. (`07`,`03`)
- [ ] **4.2 Plan loop.** Read `specs/*.md` (non-task) → create/update
  `specs/tasks/*.md`: decompose to smallest-checkable, assign ids, wire `deps`,
  write `acceptance`. *Acceptance:* a sample spec yields well-formed task files
  covering its requirements. (`02` §lifecycle)
- [ ] **4.3 Plan-completeness check.** "Complete enough" = every spec requirement
  maps to ≥1 task (not provably perfect). *Acceptance:* uncovered requirement
  detected; covered plan passes. (`06` §plan-completeness — *judgment method is
  OPEN in spec; pick one*)
- [ ] **4.4 TDD `test` agent loop (always-on).** For each task: ensure a **red**
  acceptance test exists — reuse if red; if a test already **passes** → mark task
  `done`, **skip build**; else write minimal red test. Author ≠ implementer.
  *Acceptance:* the three branches behave as specified. (`07` §test agent)
- [ ] **4.5 Per-task build flow test→build→verify.** Wire 4.4 → build loop(s) →
  test gate, per task. *Acceptance:* a task drives red→green→verified. (`07`,`06`)
- [ ] **4.6 Split pass (fresh).** Tiny fresh agent: given a
  `blocked: context-overreach` task + handoff → emit 2–4 smaller task files.
  Reuses `plan` settings. *Acceptance:* an over-reach task becomes valid subtasks.
  (`06` §refinement)

## Phase 5 — Orchestration (bare `flanders`)  `[depends: 4]`

- [ ] **5.1 Phase machine** `plan → build` with **drain then batch re-plan**: build
  marks gaps `blocked` and moves on; only when all tasks are `done|blocked` does a
  single focused plan loop resolve the blocks; then resume build. At most one
  phase switch per drain boundary. *Acceptance:* a planted gap drains → one
  re-plan → resumes, not per-gap bouncing. (`06`)
- [ ] **5.2 Full autonomy after launch** — no per-cycle approval; pause only on
  guardrail halt or usage wait. *Acceptance:* pipeline runs plan→build→done with
  no human gate. (`06`,`05`)
- [ ] **5.3 Termination + summary.** Success when test=0 AND all tasks `done` AND
  guardrails clear → report tasks/cost/iterations/duration. *Acceptance:* summary
  emitted on completion. (`06`)

## Phase 6 — TUI  `[depends: 2 (events), 5 (state to render)]`

- [ ] **6.1 Bubble Tea infra.** Harness emits events/state on a channel → BT
  messages (Elm model/update/view); handle resize; truecolor Lipgloss palette +
  semantic roles (`04-tui.md` table) with `[tui].theme` overrides (OPEN keys).
- [ ] **6.2 Header bar** — app · phase · **persistent `⚠ PERMISSIONS BYPASSED`**
  (red, bold/inverse, never dimmed — LOCKED req from `03`) · `iter n/max` · run
  state (RUNNING|PAUSED|WAITING|HALTED|DONE).
- [ ] **6.3 PLAN pane** — derived checklist with live markers `[ ]/[~]/[x]/[!]`,
  `◀` current, grouped by phase; selectable.
- [ ] **6.4 LIVE pane** — rendered from stream-json: `⏺` tool calls, `💬` text,
  `Task→` spawns; auto-scroll + scrollback on focus. (consumes 2.1)
- [ ] **6.5 AGENTS tree** — lead + subagents `name (model/effort)` with live
  status `● running`/`✓`/`✗`. (`07` visibility req)
- [ ] **6.6 METERS** — context-% bar with 75/90 trip marks (green→orange→red),
  stall `k/N`, usage countdown, cost (info-only label).
- [ ] **6.7 Controls** — `p` pause(after current loop) · `s`/`S` stop(graceful/
  hard) · `i` intervene(write operator-notes for **next** loop, no live steer) ·
  `j` journal · `tab` focus · `↑↓/PgUp/PgDn` scroll · `enter` task detail · `?`
  help · `q` quit. (`04` Controls)
- [ ] **6.8 WAITING (usage) view** — header `WAITING` + live countdown; stays open;
  auto-resumes at reset. (`04`,`01`)
- [ ] **6.9 Journal view** (`j`) — history list; drill into a loop's full
  stream-json. **6.10 Task detail** (`enter`) — frontmatter + body + loop history.
  **6.11 Help** (`?`).
- [ ] **6.12 Derived checklist generator.** Generate `IMPLEMENTATION_PLAN.md` from
  `specs/tasks/*.md` (nested `- [ ]/- [x]`), never hand-edited. *(This is the
  generator that supersedes the bootstrap nature of this file — see meta-note.)*
  (`02` §derived checklist)
- [ ] **6.13 `--no-tui` / non-TTY headless** mode: structured progress lines from
  the same event stream (auto when stdout isn't a TTY). (`04`; log format OPEN)

## Phase 7 — Discuss (interactive)  `[depends: 2,6]`

- [ ] **7.1 Interactive session.** Long-lived bidirectional
  `--input-format/--output-format stream-json`; keeps context (only non-Ralph
  mode). Discuss agent (`opus/high`), tools scoped to `specs/`, bypass perms.
  *Acceptance:* a turn round-trips and writes a spec edit live. (`05`)
- [ ] **7.2 Spec-authoring conventions + user-owns-granularity.** Agent follows
  house style (numbered single-concern files, Status line, `OPEN` markers,
  cross-refs, captured rationale) and **must not impose its own detail level** —
  proposes and follows the user's chosen granularity. (`05`)
- [ ] **7.3 Discuss chat TUI view** — CONVERSATION + SPECS list (`· new`/`· edited`),
  inline `⏺ Edit specs/...`, `d` diff last write, `p` hand off to plan, `esc`
  exit. Reuses palette/infra. (`05`)
- [ ] **7.4 Handoff** — discuss never auto-runs plan; on exit may *suggest* "run
  `flanders plan`"; running it is the human's only control point. (`05`,`06`)

## Phase 8 — CLI surface, polish, E2E  `[depends: all]`

- [ ] **8.1 Command surface** — `flanders discuss|plan|build|init` + bare
  `flanders` (orchestrate). Per-run flag overrides for model/effort (OPEN in `03`).
- [ ] **8.2 Operator-notes (intervene) plumbing** — define the notes file
  path/format (currently unspecified — see Findings) and fold it into the **next**
  loop's prompt. (`04` `i`)
- [ ] **8.3 End-to-end test** — a tiny fixture target project driven through
  plan→build→done against a stub/recorded `claude` (so CI needs no live CLI).
- [ ] **8.4 `AGENTS.md`** (operational only: how to build/test/run) + brief README.
  Keep status/progress in *this* file, not `AGENTS.md` (PROMPT rule).
- [ ] **8.5 Versioning** — first green build → git tag `0.0.1` (PROMPT rule:
  start `0.0.0`/increment patch).

---

## Findings — spec gaps & inconsistencies (for the plan/discuss loop to resolve)

1. **Stream-json contract was undefined** → authored `specs/08-stream-json-protocol.md`
   (draft; wire shapes marked OPEN until pinned vs CLI 2.1.x). *Highest technical
   risk in the project.* See task **2.1–2.3**.
2. **`state.json` was undefined** → authored `specs/09-state-and-resume.md` (draft).
   See task **1.5 / 3.12**.
3. **`flanders init` inconsistency** — referenced in `03-config.md` ("missing →
   `flanders init` …") but **absent from the command surface** in `00-overview.md`.
   Reconcile (add `init` to the surface, or fold default-writing into bare run).
4. **Operator-notes file undefined** — `04-tui.md` `i` writes "an operator-notes
   file the harness folds into that loop's prompt," but no path/format in
   `03-config.md`. Define it (task 8.2) — candidate `[paths].notes`.
5. **Model→context-window table is OPEN** (`03-config.md`) but **required** to turn
   token counts into a % when `window_tokens = 0` (task 2.2). Either ship a small
   model→window map or require the number; decide before 2.2. (Loader side ready:
   `config` already accepts `window_tokens = 0` as the "auto-detect by model"
   sentinel; only the model→window map itself remains for 2.2.)
6. **Plan-completeness method is OPEN** (`06`) — agent self-assessment vs. coverage
   check vs. (rejected) user approval. Pick one for task 4.3.
7. **Stale spec note** — multiple specs say "the harness's own directory is not yet
   a git repo"; it now **is** (branch `main`, one commit). Minor cleanup for a
   future discuss pass; doesn't block anything.
8. **OPEN items that don't block a first build** (track, don't gate): permission
   mode default (locked to bypass anyway), guardrail recovery UX (`01`/`06`),
   single-screen vs full-screen LIVE (`04`), `--no-tui` log format, `tdd=false`
   escape hatch (`07`), test-command auto-detect (`03`), how the test agent locates
   an existing test (`07`), optional task frontmatter `attempts` (`02`).

## Build conventions

- Module `flanders`; packages under `src/`, imported as `flanders/src/lib/<pkg>`. Build/test/vet/run commands are in AGENTS.md. First green tag: `0.0.1`.
- External dependencies: `github.com/BurntSushi/toml v1.4.0` (config parsing, TOML);
  `gopkg.in/yaml.v3 v3.0.1` (task-file frontmatter, YAML — node-based for lossless
  round-trip). Config is TOML, task files are YAML — both by spec design.

## Working agreements (from PROMPTs)

- Search before assuming missing; **don't reimplement** — consolidate in `src/lib`.
- One unit of work per loop; run the **relevant** tests after each change; keep
  this file current via a subagent; commit + push + tag on green.
- Single source of truth, **no migrations/adapters**; fix unrelated failing tests
  as part of the increment; finish features (no stubs/placeholders).
