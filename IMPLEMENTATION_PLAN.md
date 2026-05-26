# Flanders — Implementation Plan

> **Status:** Phase 0 (project foundation) COMPLETE; Phase 1 COMPLETE — all of
> 1.1–1.6 done. Phase 2.1–2.4 COMPLETE — stream-json parser, live
> context-occupancy tracker, usage-limit detection + reset parse, CLI invocation
> builder (`src/lib/invoke`). Go module (`module flanders`, go 1.24), layout
> (`src/cmd/flanders` + `src/lib/{paths,logging,config,task,state,journal,stream,
> invoke}`), file-backed slog logger, paths helper, config loader + default-file
> writer, the task-file model, the task store/selector, the state cache, the
> journal, the stream-json parser (typed events + `LoopObservation` aggregator,
> fixture tests vs. real `claude 2.1.150`), and the live `Tracker` all exist with
> passing tests. `src/cmd/flanders/main.go` has a real dispatcher: `init` → writes
> a commented default config; bare `flanders` → orchestrate startup;
> `discuss|plan|build` → honest "not implemented yet" error; unknown → usage error.
> `Version` const is `0.0.11`; tag `0.0.11` exists. `go build/vet/test ./...` all
> green.
>
> **AUDIT (2026-05-25):** a full re-audit of `src/lib/*` against `specs/*` (8
> parallel agents + Opus synthesis) confirmed every "done" item's spec claims hold,
> and surfaced a small set of **confirmed corrective gaps** now folded in below —
> the most important being **0.5** (`[paths]` config is parsed but `paths.New`
> ignores it → the whole `[paths]` section is a silent no-op; spec-03
> non-compliance). Phase-3/5/7 items were enriched with scoping clarifications the
> specs require but the items hadn't yet spelled out (in-loop store hot-reload,
> stall-counter reset, `max_cycles` accounting, soft-wind-down fallthrough,
> reconciliation order). New discuss/plan gap items added (3.x notes, 4.3 method,
> 7.5–7.6). See the expanded **Findings** section.
>
> **RE-VERIFY (2026-05-25, independent pass):** a second audit (5 spec-summary
> agents + 6 code-verification agents over `src/*` vs `specs/*`) re-confirmed,
> against the actual source, every "done" claim AND every open-gap item below.
> Build is green: `go build/vet/test ./...` pass with **103 tests, 0 failures, 0
> skips, no TODO/FIXME/panic/stub markers** anywhere in `src/`. Code-confirmed
> facts now pinned: **2.5** — zero `os/exec` in the tree, no supervisor exists (top
> blocker, confirmed); **0.5** — `paths.New` (`paths.go:48-66`) hardcodes the
> `Default*` constants AND `runOrchestrate` (`main.go:107-165`) **never calls
> `config.Load` at all**, so paths *and* the log level (hardcoded `slog.LevelInfo`,
> `main.go:125`) run on defaults regardless of config — the fix is broader than a
> `[paths]` overlay, it is "load config at startup and resolve everything through
> it" (folds in finding 15); **2.6** — no outbound encoder, `rate_limit_event`
> epoch can regress (`observe.go:254-256` unconditional overwrite), 9 spec-08
> fields RAW-only; **1.7** — no id↔filename check, `keyAttempts` const w/o
> accessor, `Notes()/Files()` getters but no setters; `[tui]` and `[logging]`
> config sections both absent (findings 14/15). No new gaps surfaced; no progress
> has landed since 2.4 (no commits), so the priority order is unchanged.
>
> **RE-VERIFY (2026-05-25, third pass @ commit `e79618b`):** a third independent
> pass (3 parallel code/spec agents) re-confirmed, against the live source, every
> "done" claim (0.1–0.4, 1.1–1.6, 2.1–2.4 genuinely meet acceptance — no hidden
> stubs) AND every open gap (0.5, 1.7, 2.5, 2.6, `[tui]`/`[logging]` sections — all
> still open, none secretly fixed). `go build/vet/test ./...` green; `Version`
> `0.0.11`, tags `0.0.1`–`0.0.11` present. NOTE: the whole AUDIT/RE-VERIFY block
> above (and most of the Findings section) is **uncommitted** on `main` — the
> auto-commit hook tags source but did not commit these plan edits; keep them.
> **0.5 → Phase 3**.
>
> **PROGRESS (2026-05-26):** **2.5 process supervisor DONE** (`src/lib/supervise` — spawn/stream/wait, process-group timeout-kill, stdin injection writer; 12 tests, race-clean) plus **2.6(a)** outbound encoder (`stream.EncodeUserMessage`) and **2.6(b)** reset-epoch regression guard. The Phase-3 loop-engine blocker is cleared. `Version` `0.0.12`, tag `0.0.12`. `go build/vet/test ./...` green.
>
> **PROGRESS (2026-05-26):** **0.5 DONE** — config is now loaded at startup and the `[paths]`/`rules_file` overlay applies via `paths.NewFromConfig` (`runOrchestrate` loads `.flanders/config.toml`, missing → defaults, invalid → hard error). Log level left non-configurable pending a `[logging]` spec section. `Version` `0.0.13`, tag `0.0.13`.
>
> **PROGRESS (2026-05-26):** **3.1 iteration driver DONE** (NEW package `src/lib/loop`). `loop.Driver.Iterate(ctx, phase) (*Result, error)` runs steps 1–4 of the 8-step anatomy as a fresh-context pass: **select** (`task.LoadDir` rebuilds the store from disk at the TOP of every iteration — the audit's required store hot-reload, so an in-loop split is visible to the very next loop), **compose** (minimal: the task file verbatim + a one-line plan done/left summary; the richer dep-outcome/spec-excerpt composition stays 3.2), **spawn** (`invoke.Build` + `supervise.Run`, fresh `--session-id` per loop, `[guardrails].iteration_timeout` + `stream_input` wired through), **observe** (fold `LoopObservation`, `Classify(exitCode)`, then `journal.Append` a Summary + the verbatim transcript captured via `supervise.Spec.RawSink`). Returns a rich `Result{Phase,Task,NoWork,AllDone,SessionID,Observation,Outcome,ExitCode,TimedOut,JournalSeq,Duration}` so the orchestrator acts without re-deriving. DELIBERATE non-scope (each its own item, slotting into the same Iterate spine): status mutation/reconcile stays the agent's + 3.5's (the driver only READS status before=pending / after=reload-from-disk to record the journal transition — it never writes status); state.json run-state machine (iter/stall/usage/phase) stays the orchestrator's (Phase 5); verify/test-gate (3.4), git checkpoint (3.6), guardrails (3.8–3.12). Testing seams = unexported fields `run`(→`supervise.Run`)/`newSessionID`/`now`, swapped in same-package tests; 7 tests incl. one that runs the REAL supervisor over a `cat <fixture>` stub (no live `claude`, per 2.5) to exercise RawSink→journal + the fold end-to-end. NOT yet wired into `main.go runOrchestrate` — Phase 5 owns the loop that calls Iterate. `go build/vet/test ./...` green, race-clean. `Version` `0.0.14`, tag `0.0.14`.
>
> **PROGRESS (2026-05-26):** **3.4 test gate (verify step) DONE** — NEW package `src/lib/verify` (`verify.Run(ctx, dir, config.Commands) Result`) is the harness-owned ground-truth gate (spec 01 §verify/§done-detection): runs `[commands].test` + optional `build`/`lint` via `sh -c` in the project root, captures exit codes + tail-bounded combined output. A non-zero exit is DATA (`CommandResult.ExitCode`), never a Go error — only a start/cancel failure sets `CommandResult.Err`. `Result.Passed()` = the canonical done-gate (TEST command exits 0, the only command spec 01 §done-detection names); `Result.OK()` = stricter all-ran-passed (build/lint count too) for callers that want it. Wired into `loop.Driver.Iterate` as step 5 (after observe/classify, before journaling): gated to code-producing phases on a clean invocation via `runsTestGate(phase, outcome)` = `outcome==OutcomeSuccess && (phase=="build"||phase=="test")` — plan/split/discuss produce specs not code (judged by 4.3), and a usage-limit/error/timeout loop didn't finish so verifying a half-done tree would confuse done-detection/stall. The test result is recorded in the journal `Summary.Test` (existing field, no schema change — build/lint results surfaced on the in-memory `loop.Result.Verify` + logged, not journaled, since spec 01 §journal names "test result" singular) and the full verdict surfaced on `loop.Result.Verify *verify.Result` (nil when the gate didn't run). This UNBLOCKS done-detection (3.7: `Verify.Passed() && AllDone && no-stall`) and status reconciliation (3.5). 10 verify tests + 4 new loop tests (gate passes/fails reflecting real exit code, plan-phase skip, non-success skip); existing loop tests stay green because `config.Default()` leaves `Commands.Test=""` → gate skips → `Test.Ran=false` as before. `go build/vet/test -race ./...` all green. `Version` 0.0.15, tag 0.0.15.
>
> **PROGRESS (2026-05-26):** **3.5 status reconciliation DONE** — two NEW packages plus loop wiring. `src/lib/git` (read-only: `IsRepo`/`HeadSHA`/`WorkingChanges`/`Snap`/`Diff`) is the harness's window onto the target's working tree — the "did work happen this loop + which files" signal (spec 02 §Mutation ownership, spec 09 §resume). Best-effort/never-fatal: a non-repo target yields an inert `Snapshot{IsRepo:false}` → no signal, callers lean on the test gate. Reads ONLY (no commit/stage/init — that is 3.6) so a status-inference path can't mutate the repo it measures. `src/lib/reconcile` (`Reconcile(t, Signals{TestRan,TestPassed}) (Result, error)`) is the harness's half of the locked mutation split: precedence is **agent-status-first** (an agent-written `done`/`blocked` is `ActionRespected` verbatim — the run-level done-gate 3.7 catches a false done, not per-task reconcile), then inference fallback (non-terminal + test gate ran & passed → `ActionPromoted` to `done`, the "never depend on the agent remembering" path), then normalize a stuck `active`→`pending` (`ActionNormalized`, since the selector only re-picks pending; e.g. a killed/timed-out loop), else `ActionUnchanged`. Imports only `task` (test verdict arrives as plain bools; git's work-signal is NOT a reconcile input — it can't prove a task done, so it feeds the stall guardrail/journal, not the status decision). Wired into `loop.Driver.Iterate` as step 6 (evaluate): snapshot git before spawn + after the loop → `git.Diff` for `WorkHappened`/`FilesTouched` (recorded in journal `Summary.Files`), re-read the task as the agent left it, `reconcile.Reconcile` (writes the file iff status changed). New `loop.Result` fields: `Reconcile reconcile.Result`, `WorkHappened bool`, `FilesTouched []string`. `buildSummary` now takes the reconciled `after` task + files (one reload, not two) so journal StatusAfter/Reason reflect the NET transition (agent flip | harness promotion | normalization). 8 git tests + 8 reconcile tests + 5 new loop tests (promote-on-passing-gate, normalize-active, respect-blocked, records-git-work in a real repo, non-repo-no-signal); existing loop tests stay green. `go build/vet/test -race ./...` all green. `Version` 0.0.16, tag 0.0.16. DEFERRED (documented, not stubbed): the RUNNING-crash *resume* reconcile entry point (spec 09 §resume: on resume of a crashed RUNNING state, re-derive status from git before continuing) stays for Phase 5 — its only consumer is the orchestrator, which doesn't exist yet; building it now would mean guessing the orchestrator's resume interface. The post-loop `reconcile.Reconcile` it will reuse is built and tested.
>
> **PROGRESS (2026-05-26):** **3.2 prompt composition DONE** (`src/lib/loop/compose.go`). The composer is now the full cost/quality lever (spec 01 §Prompt composition), no longer minimal. `composePrompt` became a `*Driver` method (it resolves referenced spec files against `d.paths.Root` and debug-logs skipped refs; the per-section builders stay pure free functions). It now emits, in the spec's order: (1) current task file verbatim, (2) **dependency outcomes** — for each `dep`, `store.ByID` resolves it and emits a compact FRONTMATTER-derived summary (id, status, the `acceptance` it met, plus `files`/`notes` when present), never the dep's body (injecting a whole second file would defeat the lever), (3) **referenced spec excerpts** — `specRefRe` parses `specs/x.md §Section` refs out of the task body (any `.md` path so a configured non-`specs/` layout still works; `§` is the intentional-reference signal), `readSpecSection` reads the file (refusing absolute/`..`-escaping paths) and `extractSection` returns the named heading through the next same-or-higher heading, skipping headings inside ``` fenced code blocks; an unresolvable ref (missing file/section) is debug-logged and skipped, never fatal, (4) one-line plan summary (unchanged). Heading matching is fuzzy-but-safe (`headingMatches`/`normalizeHeading`: exact, or either side a prefix of the other) to absorb trailing punctuation and parenthetical heading suffixes. 7 compose tests (task-file+summary, dependency-outcomes-not-body, unrelated-task-excluded, referenced-excerpt-included/sibling-excluded, unresolvable-ref-skipped, `extractSection` fence/prefix/boundary unit test) + existing loop tests green. `go build/vet/test -race ./...` all green. `Version` 0.0.17, tag 0.0.17.
>
> **PROGRESS (2026-05-26):** **3.6 git checkpointing DONE** — the WRITE side of `src/lib/git` plus loop wiring. `src/lib/git` gains `Init`/`AddAll`/`Commit` and a `Checkpoint(ctx, dir, msg) (sha, committed, err)` convenience (stage-all → commit; a clean tree is a no-op returning committed=false, never an error, so "no changes" is never mistaken for "commit failed"); the package doc is rewritten from "read-only" to "two sides (read signal + write checkpoint), separated by function not file — reconcile still calls only the read side". NEW `src/lib/loop/checkpoint.go`: `Driver.checkpoint(ctx, phase, iter, before, after, vr) string` is step 7 of the anatomy, wired into `Iterate` after the journal append. "Progress" = a status change (`before.Status() != after.Status()`; the selector always hands a `pending` task so this is a flip/promotion/normalization) OR a passing test gate (`vr.Passed()`); `commit_each` modes honored: `progress` (default, commit on progress), `iteration` (commit any tree change), `off`/`enabled=false` (never). `init_if_missing` is the autonomous-run consent: a non-repo target is `git init`'d (the config flag governs at the engine level; an interactive "offer" is a Phase-6 TUI concern). `message_tmpl` rendered via `renderCheckpointMessage` ({phase}/{iter}/{task}/{result}; {result}=resulting task status; unknown placeholders left verbatim). Best-effort/never-fatal: the journal entry is already written and work is on disk, so a commit failure (e.g. no git identity) is logged and yields "". `Iterate` signature gained an `iter int` param (the orchestrator's per-phase counter, used only for the {iter} message var — the driver owns no counter); new `Result.Checkpoint string` (new commit sha, or "") for the orchestrator to persist as `state.last_checkpoint` (spec 09). `setupProject` test helper now disables git so the 14 pre-existing loop tests are unaffected (the read-side git signal is independent of `[git].enabled`); 9 new checkpoint tests (commit-on-status-change with exact templated message, commit-on-passing-gate, skip-without-progress, iteration-mode-commits-any-change, off/disabled never commit, init_if_missing creates repo + commits, init-disabled no commit, message-template unit incl. unknown-placeholder). `go build/vet/test/vet -race ./...` all green, 14 packages. `Version` 0.0.18, tag 0.0.18.
>
> **PROGRESS (2026-05-26):** **3.7 done-detection, 3.8 max-iterations guardrail, 3.9 stall guardrail DONE** — NEW package `src/lib/guardrail` (`guardrail.go` + `guardrail_test.go`, 6 test functions / 21 subtests, all green; `go build/vet/test -race ./...` green across 15 packages). Pure package (imports nothing), mirroring the `src/lib/reconcile` and `src/lib/verify` pattern of building tested primitives ahead of the Phase-5 orchestrator. Five functions: `Changed(filesChanged, statusChanged bool) bool` (progress = touched files OR changed task status); `StallStep(prev int, changed bool) int` (stall-counter transition: reset to 0 on progress, else prev+1 — both increment AND reset tested end-to-end); `StallTripped(count, n int) bool` (trips when count>=n; n<=0 disables); `MaxIterationsReached(iter, max int) bool` (per-phase cap; iter>=max inclusive; max<=0 disables; per-phase-vs-global apportionment deferred to the orchestrator as spec-06 OPEN); `Done(testPassed, allTasksDone, stalled bool) bool` (the locked spec-01 done-detection rule — all three required; agent completion report deliberately NOT a parameter). NOT wired into main.go — same pattern as 3.1/3.4/3.5/3.6; the Phase-5 orchestrator owns the run-state machine and all stall/iter state. `Version` 0.0.19, tag 0.0.19.
>
> **PROGRESS (2026-05-26):** **3.3 loop rules file DONE** — NEW package `src/lib/rules` (`rules.go` + `rules_test.go`, 5 tests). `rules.DefaultMarkdown` is the canonical loop-rules text (the agent's behavioral contract appended via `--append-system-prompt` every loop, spec 01 §invocation): the five spec-derived rules — (1) one unit of work per loop, (2) flip your task's `status` (your one structured edit; `done` advisory vs. the harness test gate; spec 02 §mutation ownership), (3) never hand-edit harness-owned state (`.flanders/state.json`, `.flanders/journal/`, `IMPLEMENTATION_PLAN.md`), (4) delegate exploration/search to subagents (the primary context-pressure lever, spec 01 §prompt-composition), (5) proactive context-overreach handoff + the in-loop-split distinction (spec 01 §guardrails tier 1, spec 06 §refinement). `rules.WriteDefault(path)` mirrors `config.WriteDefault` (atomic temp+rename, NEVER overwrites a user's tuned file). Wired two ways: `flanders init` (`initAt` in `src/cmd/flanders/main.go`) now materializes `.flanders/rules.md` alongside config.toml so the user can read/tune it; and `loop.readRules` now falls back to `rules.DefaultMarkdown` when the file is absent (was `""`), so the loop discipline is ALWAYS in force — a project that skipped `init`, or whose rules file was removed, still gets the rules (mirrors config's default-fallback philosophy). Anti-drift test locks that all five required rules remain present in DefaultMarkdown. This also enables 3.11 tier 1 (the proactive-handoff tier is rule-driven). `go build/vet/test -race ./...` all green across 16 packages; `Version` 0.0.20, tag 0.0.20.
>
> **PROGRESS (2026-05-26):** **3.11 context-pressure three-tier guardrail DONE** — NEW file `src/lib/loop/context_pressure.go` (`contextGuard`) wired into `loop.Driver.Iterate`'s spawn/observe steps, plus the `task.SetNotes` setter (`src/lib/task/task.go`) the harness handoff needs. The guard drives a per-loop `stream.Tracker` off the supervisor's `OnEvent` hook (the in-flight occupancy signal) and takes the spec-01 three-tier action against the running process: **tier 1** proactive agent handoff is rule-driven (already in `rules.DefaultMarkdown`, 3.3 — no code here); **tier 2** soft wind-down at `[context].soft_pct` injects a graceful "wrap up — self-block context-overreach, write a handoff, commit, end" message over stdin exactly once (via `Proc.Inject`, which encodes it with the 2.6a `stream.EncodeUserMessage` envelope) — but ONLY when `[agent].stream_input` is on; **tier 3** hard backstop at `[context].hard_pct` kills the process group exactly once and the harness OWNS recording the outcome — `Driver.markContextOverreach` writes `blocked: context-overreach` + a `contextOverreachHandoff` note (partial-progress files; "split fresh, the exhausted loop must not split itself") to the task file BEFORE the reconcile reload, so reconciliation respects the terminal status on the normal agent-status-first path. The journal `Summary.Error` names the backstop precisely (a hard kill otherwise reads as a generic process error to `Classify`), and `Result.ContextTrip stream.Trip` surfaces the peak tier for the orchestrator/TUI. Documented fallthrough (audit note 1): `stream_input=false` ⇒ tier 2 is skipped (no stdin channel) and the guard goes straight from tier 1 to tier 3. Concurrency: `handle` runs serially in the supervisor's single read goroutine; a mutex guards the decision flags so the post-loop reads (after `Wait` joins the goroutine) are race-free. Tests in NEW `src/lib/loop/context_pressure_test.go` (6 functions): soft-injects-once, hard-kills-once, soft-then-hard escalation, the `stream_input=false` fallthrough, stays-quiet-below-soft, the handoff-note content, plus an end-to-end `TestIterateContextHardKillMarksBlocked` that drives a real supervisor over a `cat` of a 191k-token fixture and asserts the task file lands `blocked: context-overreach` + handoff, reconcile respects it, and the journal records it. `go build/vet/test -race ./...` all green across 16 packages. `Version` 0.0.21, tag 0.0.21.
>
> **PROGRESS (2026-05-26):** **3.12 usage-limit wait/auto-resume DONE** — NEW package `src/lib/usage` (`usage.go` + `usage_test.go`, 18 tests, race-clean), the subscription usage-limit wait mechanism (spec 01 §guardrails, spec 09 §resume). Built ahead of the Phase-5 orchestrator (same pattern as `guardrail`/`verify`/`reconcile`). Pure decisions: `Decide(cfg config.Usage, cyclesUsed int) Decision` applies `[usage].on_limit` (halt wins outright) then the `max_cycles` cap (cyclesUsed >= max_cycles → halt; max_cycles<=0 = unlimited, the default; anything but "halt" takes the safe wait path); `WaitDuration(resetAt *time.Time, backoff, now) (d, usedBackoff)` (future reset → sleep until it; past reset → 0/resume-immediately; nil reset → `[usage].backoff` clamped >=0). Mechanism: `Waiter` (seams `now`/`sleep`, prod uses an interruptible `contextSleep`) with two entry points — `HandleLimit(ctx, st, statePath, resetAt)` for a freshly-detected limit (the orchestrator calls it when `loop.Result.Outcome == stream.OutcomeUsageLimit`, passing `Result.Observation.ResetAt`) and `Resume(ctx, st, statePath)` for the crash-resume path (orchestrator finds `run_state==WAITING` on startup). TWO pinned invariants: (1) **persist-before-sleep** — HandleLimit writes `WAITING` + `reset_at` + the incremented `cycles_used` to state.json BEFORE sleeping, so a crash during the (possibly hours-long) wait resumes via `Resume`; (2) **cycles_used counts windows ENTERED**, bumped at wait start (not completion) so a crash-then-Resume can't double-count (`Resume` never bumps). A cancelled ctx during the sleep returns `ctx.Err()` leaving `WAITING` on disk (graceful shutdown → restart resumes). `max_cycles=N` drains exactly N windows then halts on the (N+1)th limit; halt sets `run_state=HALTED` + `st.Halt{reason, current_task}`. NOT wired into `main.go` — the Phase-5 orchestrator owns the run loop that detects the usage-limit outcome and calls `HandleLimit`/`Resume`; this package ships the complete, tested behavior for it to call. `go build/vet/test -race ./...` green across 17 packages. `Version` 0.0.22, tag 0.0.22.
>
> **PROGRESS (2026-05-26):** **4.1 subagent-class resolution DONE** — `config.SubagentClass(name string) AgentClass` added in `src/lib/config/config.go`, the companion to the existing `config.PhaseClass`. It starts from the global `[subagents]` default (sonnet/low) and overlays any `[subagents.<name>]` table. TWO deliberate differences from `PhaseClass`: (1) an unknown name is NOT an error — subagent class names are open-ended (a lead may spawn any-named helper), so a name with no override resolves to the global default; (2) overrides merge FIELD-BY-FIELD, not wholesale — a `[subagents.<name>]` table that sets only `model` (or only `effort`) keeps the global default for the omitted field, so a partial override never blanks a field to "" (mirrors `agentClassFromMap`). The supporting structs (`Subagents`, `Subagents.Classes`, `AgentClass`) and the custom `[subagents]` `UnmarshalTOML` already existed (1.1) — only the resolver was missing. Built ahead of its consumers (same primitive-ahead-of-orchestrator pattern as guardrail/usage/reconcile): the TUI AGENTS tree (6.5, renders subagent `name (model/effort)`) and any plan/build prompt that wants to tell the lead which subagent classes to spawn. The stale "lands with plan task 4.1" note in `PhaseClass`'s doc comment was updated to point at `SubagentClass`. 2 new config tests (`TestSubagentClass` — default-for-any-name + full override + unconfigured-falls-back; `TestSubagentClassPartialOverride` — model-only and effort-only overrides each keep the default for the other field). `go build/vet/test/vet -race ./...` green across 17 packages. `Version` 0.0.23, tag 0.0.23.
>
> **PROGRESS (2026-05-26):** **4.2 plan loop DONE** — NEW `src/lib/loop/plan.go`: `Driver.PlanIterate(ctx, iter) (*Result, error)`, the plan-phase iteration (spec 02 §Plan lifecycle). ARCHITECTURAL DECISION (the 4.2 open question): a SEPARATE method, NOT a phase-branch inside `Iterate`. The plan loop shares none of Iterate's task-selected spine — it is not `store.Next()`-selected (it always operates on the whole spec set), produces task files not code (so the test gate is meaningless; `runsTestGate` already excludes `plan`), and has no single task whose status to reconcile. Branching those four steps out of `Iterate` would gut it; instead the genuinely shared half — spawn, observe, journal, checkpoint, the context guard — was factored into helpers both loops call: `Driver.commit` (extracted from `checkpoint`; takes a phase-specific progress signal + {task}/{result} message vars), `Driver.baseSummary` (extracted from `buildSummary`; the phase-agnostic journal fields), and `newContextGuard` (now takes the tier-2 wind-down message, so the plan loop injects `planWindDownMessage` — "commit your task files and end" — instead of the task loop's self-block message). `PlanIterate` does compose→spawn→observe→checkpoint: `composePlanPrompt` injects the decompose instruction (`planInstructions`) + a compact summary of existing task files (`existingTasksSummary` — id/status/deps/acceptance, never bodies; store hot-reloaded every loop so a prior loop's tasks are visible) + the non-task specs (`specs/*.md`) VERBATIM — the plan agent's input genuinely IS the whole spec set, unlike the build loop's per-task cost lever; no spec files → infrastructure error ("nothing to plan from"). Spawn uses the plan class (opus/high via `config.PhaseClass`), fresh session, context guard with `planWindDownMessage`; a tier-3 hard kill just ends the loop (no task to mark blocked → no `markContextOverreach`). Journal records phase=plan, empty Task (spec: "empty for plan/discuss"), no test gate, files=task files authored. Checkpoint (`planCheckpoint`): progress = a working-tree change (no status flip to read), message {task}="(plan)" {result}="N tasks". NOT wired into a run loop — the Phase-5 orchestrator calls PlanIterate until plan-complete (4.3, the plan-loop exit condition). BUG FIXED en route: `git.WorkingChanges` ran `git status --porcelain` (default `--untracked-files=normal`), which collapses a brand-new untracked dir to the dir entry (`specs/tasks/`) and loses per-file detail — switched to `--untracked-files=all` so the journal Files field + stall detection name individual files (the first plan loop creating the first task files in a fresh tasks dir is exactly the case that exposed it). 5 new plan tests + existing loop/git tests green. `go build/vet/test -race ./...` green across 17 packages. `Version` 0.0.24, tag 0.0.24.
>
> **PROGRESS (2026-05-26):** **4.3 plan-completeness check DONE** — NEW `src/lib/loop/plancheck.go`: `Driver.PlanComplete() (*Coverage, error)`, the plan-phase loop-EXIT condition (spec 06 §Plan-completeness criterion; spec 02 §Plan lifecycle). METHOD DECISION (the spec-OPEN judgment method, Finding 6): the **mechanical coverage scan** the audit recommended, not agent self-assessment — harness-owned ground truth, same philosophy as the test gate. Requirement unit = each level-2 (`##`) heading in a non-task spec file; a `###` sub-heading is folded in as an ALIAS of its enclosing `##` (a task referencing a finer subsection still credits the parent), NOT its own requirement (per-sub-point requirements would make the plan loop chase completeness forever — exactly what spec 06 warns against). A requirement is covered iff some task body references its spec (matched by base filename, case-insensitively) AND names its heading/alias — reusing compose.go's EXISTING fence-aware `heading`/`isFence`/`headingMatches`/`normalizeHeading` and `parseSpecRefs` (single source of truth, no duplication — this is WHY plancheck lives in package `loop`, not a new package). House-style meta sections are excluded (`defaultExcludedSections`: `OPEN`, `spec index`, `keyword index`, `remaining open decisions`, `keywords` — matched exact or `<entry> `-prefix), grounded in spec 05 §Spec-authoring conventions (OPEN = undecided, 00-overview = an index): excluding them is what keeps the scan SATISFIABLE (an `## OPEN` can't have a task). Spec-03's `#`/`##`-looking lines inside its ```toml fence are correctly skipped by the fence-aware parser. Cost asymmetry documented: a false "uncovered" costs an extra bounded plan loop (max-iter guardrail); a false "covered" lets a gap reach build where drain+batch-replan catches it (spec 06's accepted fallback) — so the scan is a FLOOR catching gross omissions, not a perfection proof. Returns rich `Coverage{Specs,Tasks,Requirements,Covered,Uncovered []Requirement}` + `Complete()` (= no gaps; vacuously true when 0 requirements; the no-spec-files case errors like composePlanPrompt). WIRED into `PlanIterate`: a new `Result.PlanComplete *Coverage` field is filled best-effort after the loop is journaled+checkpointed (a scan error — e.g. the agent wrote a malformed task file — logs + leaves it nil, never discards a recorded loop), so the orchestrator loops "while !res.PlanComplete.Complete()" and feeds `Uncovered` into the next focused re-plan. Pure core (`specRequirements`/`scanCoverage`/`requirementCovered`/`isExcludedHeading`) is unit-tested without disk; `PlanComplete`/`PlanIterate` tested end-to-end. 7 new tests (extraction+fence+nesting+exclusions, isExcludedHeading prefix matching, scanCoverage fuzzy+nested-credit+gap, Complete vacuous+full, PlanComplete end-to-end on disk, no-specs error, PlanIterate populates PlanComplete). NOT wired into a run loop — Phase-5 orchestrator is the consumer (same primitive-ahead-of-orchestrator pattern as guardrail/usage/verify/reconcile). `go build/vet/test -race ./...` green across 17 packages. `Version` 0.0.25, tag 0.0.25.
>
> **PROGRESS (2026-05-26):** **4.4 TDD test agent loop DONE** — the always-on test phase (spec 07 §The `test` agent; spec 06 §test-build-verify). ARCHITECTURE DECISION (vs. 4.2's separate `PlanIterate`): the test loop RIDES the existing `Iterate` spine rather than getting its own method, because unlike the plan loop it shares ALL of that spine — it is task-selected (`store.Next()`), composes around one task, runs the harness test gate, and reconciles one task's status. Only two things differ, both localized in `Iterate`: (a) `composePrompt` is now phase-aware (`compose.go` `taskRoleHeader` + `buildInstructions`/`testInstructions` constants) — the test agent gets the TDD contract (ensure a FAILING acceptance test; never implement; the three spec-07 branches), the build agent gets the implementer role; (b) post-loop, a green gate does NOT auto-promote in the test phase (`reconcile.Signals.TestPassed` is gated on `phase != "test"` — a green suite can mean "no test covers this acceptance yet", so only the agent's explicit gate-corroborated `done` marks satisfaction), and a new `classifyTestVerdict` (NEW `src/lib/loop/testphase.go`) maps (post-reconcile status, gate result) → `Result.TestVerdict` (NEW field): **Satisfied** (agent `done` + green gate → skip build, branch 2), **RedReady** (a red acceptance test exists → proceed to build, branches 1/3), **NoRedTest** (pending + green gate → no red test produced → re-run the test loop), **Blocked** (drain/re-plan), or **None** (non-test loop / non-clean outcome). GROUND-TRUTH CORRECTION: an unconfirmed `done` (agent claims satisfied but the suite is not green) is demoted to `pending` and routed to build (spec 00 decision 2 — never skip build on an unverified done). RESOLVED the spec-07 OPEN "how the test agent locates an existing test": the agent's status flip (it alone knows which test covers the acceptance) combined with the whole-suite gate (ground-truth red/green); in the per-task test→build→verify flow the suite is green before each test loop, so a freshly-red suite reliably means THIS task's test is red. A per-task filtered gate is a documented future refinement; spec 07 updated (Red/green determination LOCKED; the OPEN struck through). 8 new tests in `testphase.go`/`testphase_test.go` (classifyTestVerdict branch table; test-agent vs build-agent prompt; RedReady-proceeds-to-build; Satisfied-skips-build; green-gate-does-NOT-promote in the test phase (the headline difference from build); unconfirmed-done-demoted; blocked-drains; non-success-no-verdict) + the 5 existing compose tests updated to pass a phase; `go build/vet/test -race ./...` green across 17 packages. NOT wired into a run loop — the Phase-5 orchestrator (and the 4.5 per-task flow) is the consumer that calls `Iterate(ctx, "test", iter)`, reads `Result.TestVerdict`, and skips or runs the build loop accordingly. `Version` 0.0.26, tag 0.0.26.
>
> **Next up, in priority order:** With 4.2, 4.3 AND 4.4 done (the test loop is `Iterate(ctx, "test", iter)` → `Result.TestVerdict`), the open Phase-4 items are **4.5** (per-task build flow: wire the test loop → build loop(s) → verify per task — the consumer that reads `Result.TestVerdict` to skip build on Satisfied, run build on RedReady, re-run the test loop on NoRedTest, and drain on Blocked; it is the smallest orchestration unit and a natural Phase-5 building block) and **4.6** (split pass: a fresh agent turns a `blocked: context-overreach` task + handoff into 2–4 smaller tasks — `PlanIterate`/`composePlanPrompt` are a close template). The remaining Phase-3 item **3.10** (per-iteration-timeout recording) stays orchestrator-coupled. **Phase 5** (orchestration) wires plan↔build↔test, the run-state machine, and 3.8–3.12/3.10 together — the natural home once 4.5/4.6 give it the per-task flow to drive.
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
- [x] **0.5 Load config at startup + apply the `[paths]` overlay** `[CONFIRMED GAP
  — spec-03 non-compliance; code-verified twice]`. Two coupled facts: (a)
  `paths.New(root)` (`src/lib/paths/paths.go:48-66`) hardcodes the `Default*`
  constants and **never consults `config.Paths`**; and (b) — the deeper root cause
  — `runOrchestrate` (`src/cmd/flanders/main.go:107-165`) **never calls
  `config.Load` at all** (the `config` import is used only by `initAt`'s
  `WriteDefault`). So a user's `[paths]` section is parsed+validated by
  `src/lib/config` and then ignored, AND the log level is hardcoded
  (`slog.LevelInfo`, `main.go:125`) because no config ever reaches it. Spec 03 says
  `[paths]` (and the rest) are configurable. *Fix:* (1) in `runOrchestrate`, load
  `.flanders/config.toml` when present (fall back to `config.Default()` when
  absent) — this is the single startup config-load every later phase depends on;
  (2) add a config-aware constructor in `src/lib/paths` (e.g. `NewFromConfig(root,
  *config.Config)` or `New(root, opts)`) that overlays any non-empty
  `[paths].{specs,tasks,journal,plan,state}` onto the defaults, keeping the rules/
  config/log derivations; (3) resolve the logger level from config once loaded
  (this is the natural home for finding 15 — but it needs a `[logging]` level field
  added to `src/lib/config`, currently absent; do the field+wiring or leave the
  level alone and just land paths — decide). **No new package** — consolidate in
  `src/lib/paths`. *Acceptance:* a config with custom `[paths]` resolves
  journal/state/tasks to the configured locations; absent keys keep the defaults;
  startup loads the real config. Do **before** Phase-3 consumers start resolving
  paths so they never bake in the wrong locations. (Low effort, foundational.)
  (Implemented `paths.NewFromConfig(root string, cfg *config.Config) (*Paths, error)` in `src/lib/paths/paths.go`; `New(root)` now delegates to `NewFromConfig(root, nil)` (defaults-only, used by `flanders init`, which writes the default config before one exists to read). `NewFromConfig` overlays any non-empty `[paths].{specs,tasks,journal,plan,state}` AND `[agent].rules_file` onto the `Default*` constants — closing the same silent-no-op bug for `rules_file` too (the audit only named `[paths]`, but `rules_file` had it as well). Empty/whitespace-only config values keep the default (default-then-overlay contract); absolute config locations are honored verbatim (a user may point outside root), relative ones resolve against root. Config/Log/`.flanders/` are NOT configurable — fixed under root so the harness can always find where it loaded config from. `runOrchestrate` (`src/cmd/flanders/main.go`) now calls `loadConfigOrDefault(root)` FIRST, then `paths.NewFromConfig(root, cfg)`. `loadConfigOrDefault`: loads `.flanders/config.toml` (path resolved via the default layout, since the config file's own location isn't configurable); a MISSING file falls back to `config.Default()` (bare `flanders` before `init` must still run); a present-but-INVALID config is a HARD error (never silently run on defaults when the user asked for something specific). Uses `errors.Is(err, fs.ErrNotExist)` — works because `config.Load` wraps the not-exist error with `%w`. DECISION on the log level (plan item 0.5 part 3): left at `slog.LevelInfo`, NOT made configurable — spec 03 has no `[logging]` section, so adding a config field would be speculative spec-extension; deferred to the dedicated `[tui]`/`[logging]` config-section pass (findings 14/15). A code comment at the logger-init site documents this. Tests: `src/lib/paths/paths_test.go` adds TestNewFromConfigOverlaysPaths, TestNewFromConfigEmptyKeepsDefaults, TestNewFromConfigAbsolutePath, TestNewFromConfigNilMatchesNew; `src/cmd/flanders/main_test.go` adds TestLoadConfigOrDefaultMissing/Present/Invalid. End-to-end smoke test verified: a config with `journal = "logs/loops"` makes the running binary create+use `logs/loops` instead of `.flanders/journal`. `go build/vet/test ./...` all green, race-clean. Version bumped to 0.0.13.)

## Phase 1 — Config & data model (`src/lib` core)  `[depends: 0]`

- [x] **1.1 Config loader.** Parse `.flanders/config.toml` → typed struct with the
  full schema in `03-config.md` (`[paths] [commands] [agent] [phases.*]
  [subagents] [context] [guardrails] [usage] [git]`). Apply documented defaults;
  **error if `[commands].test` missing for build**. *Acceptance:* loads the sample
  config and a minimal config (defaults fill in); missing test command rejected
  for build phase. (`03-config.md`)
  (Implemented in `src/lib/config` (package `config`): `Config` struct mirrors every `03-config.md` section; `Default()` returns all documented defaults; `Load(path)` overlays the file on top of `Default()` (absent keys keep defaults, present keys win); `Validate()` checks enums/ranges; `ValidateForBuild()` enforces the required `[commands].test`. TOML library decision RESOLVED: `github.com/BurntSushi/toml v1.4.0` (mature/stable; supports `encoding.TextUnmarshaler` for duration fields and a custom `UnmarshalTOML` for the mixed `[subagents]` section). `[commands].test` intentionally has NO default (a default would make "missing" undetectable); it is enforced by `ValidateForBuild`, not `Load`. Duration fields (`iteration_timeout`, `backoff`) parse into a `config.Duration` (wraps `time.Duration`). Per-class subagent overrides (`[subagents.<name>]`) are parsed into `Subagents.Classes` (forward-compat; OPEN for v1). All config tests pass; `go build/vet/test ./...` green.)
- [x] **1.2 `flanders init`.** Write a commented default `config.toml` when absent.
  *(Note: `init` is referenced in `03-config.md` but absent from the command
  surface in `00-overview.md` — see Findings; now resolved.)* *Acceptance:* `init`
  produces a loadable, commented config.
  (Implemented in NEW file `src/lib/config/write.go` (same package `config`):
  `const DefaultTOML` (the canonical commented template, mirrors spec 03's
  "Proposed file" verbatim) + `func WriteDefault(path string) (wrote bool, err
  error)`. WHY a hand-authored template string and not an encoder dump: the
  BurntSushi TOML encoder cannot emit comments, and the comments ARE the value of
  an init file. So `DefaultTOML` is the single canonical default-file text, and
  `TestDefaultTOMLMatchesDefault` parses it back and asserts it equals `Default()`
  (plus the `[commands]` starters) — an anti-drift lock so the template can never
  silently diverge from the documented defaults.
  KEY DECISION: the `[commands]` values in `DefaultTOML` (`test = "go test ./..."`,
  `build = "go build ./..."`) are STARTERS for a Go project, NOT overlay-defaults.
  `config.Default()` is deliberately left unchanged: `test` has no overlay-default
  (required, detect-missing) and `build` overlay-defaults to `""` (omitted optional
  build = skip the compile check, parallel to `lint`). So `init` writes a runnable
  Go starter while `Default()` stays safe/stack-agnostic. The spec (03) has been
  clarified to make this starter-vs-overlay-default distinction explicit (see
  Finding 3 below).
  `WriteDefault` NEVER overwrites an existing config (returns `wrote=false`, no
  error) — `init` is for the missing-config case; a user's edits are precious.
  Write is atomic (temp-in-same-dir + rename, same discipline as
  `state.Save`/`task.WriteFile`); creates parent `.flanders/` via `MkdirAll`.
  Command dispatch added to `src/cmd/flanders/main.go`: a thin pure `dispatch(args)`
  switch — `init` → `runInit` → `initAt(root, w)` (factored out so it is testable
  against a temp dir without chdir); bare `""` → `runOrchestrate()` (the former
  `run()` startup, renamed); `discuss|plan|build` → honest "not implemented yet"
  error; unknown → usage error. No CLI framework (stdlib only).
  Tests: `src/lib/config/write_test.go` (round-trip/anti-drift lock, write-creates,
  no-overwrite, no-temp-residue, empty-path) + `src/cmd/flanders/main_test.go`
  (dispatch unknown/forthcoming, `initAt` writes loadable+build-ready config,
  idempotency). `go build/vet/test ./...` all green. Acceptance ("init produces a
  loadable, commented config") met — the generated config passes `Load` +
  `ValidateForBuild`. `Version` const bumped to `0.0.7`.)
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
- [x] **1.4 Task store / selector.** Enumerate `specs/tasks/*.md`; select the next
  actionable task = `pending` with **all `deps` `done`**; never select a task with
  unmet deps. Detect dependency cycles. *Acceptance:* selector returns correct
  next task across dep graphs; cycle surfaced as error. (`01` §select, `02` §deps)
  (Implemented in `src/lib/task/store.go` (same package `task`, NOT a new package —
  it operates directly on `*Task` and the prompt's "consolidate in `src/lib`" rule
  argues against a thin wrapper package; `task.Store` reads naturally). API:
  `LoadDir(dir)` globs `*.md`, parses+`Validate()`s each (fail-fast on the first
  malformed file, with path), builds the store; a MISSING tasks dir is NOT an error
  → empty store (the expected pre-plan state). `NewStore([]*Task)` is the test/state-
  rebuild seam. `Store.Next() (*Task, error)` returns the lowest-id `pending` task
  whose deps all resolve to `done`; returns `(nil,nil)` when nothing is actionable.
  `AllDone()` distinguishes "finished" (Next nil + AllDone) from "stalled" (Next nil
  + !AllDone). `Validate()` does the cross-task graph check (unknown deps + cycles);
  `CheckCycles()` is the standalone 3-color DFS. KEY DESIGN — id normalization lives
  HERE, not in `task.go`: because task.go stores `id`/`deps` verbatim to round-trip
  zero-padding, the store owns collapsing `0007`/`7`/`07` to one key via `normID`
  (trim space; strip leading zeros from all-digit ids, keeping a lone `0`); it is the
  ONLY place ids are compared, so a dep `0001` resolves to task `1`. Cycles are an
  ERROR not a silent nil (a cycle would otherwise masquerade as a finished plan) —
  `Next` runs full-graph cycle detection first and returns `*CycleError` naming the
  loop. Typed errors: `*CycleError` (with `Cycle []string`), `*UnknownDepError`,
  `*DuplicateIDError` (two files → same normalized id, rejected at load). Selection
  order is numeric-when-both-numeric (so 2 < 10), lexicographic otherwise, fixed once
  at load. NOTE for 1.5: `NewStore` is the rebuild entry point; an unknown dep makes a
  task non-actionable in `Next` (skipped) but is only surfaced as an error by the
  explicit `Validate()`, so `Next` stays robust on a half-built plan. 13 new tests +
  full suite green.)
- [x] **1.5 State persistence** (`state.json`, `09-state-and-resume.md`). Atomic
  write (temp+rename) on every transition; load on startup; rebuild from task
  files+journal+git when missing/corrupt. *Acceptance:* round-trip; corrupt file
  recovers without crashing.
  (Implemented in NEW package `src/lib/state` (stdlib `encoding/json` only — NO new
  external dep). `State` struct mirrors the spec-09 schema exactly (schema_version,
  phase, run_state, started_at/updated_at, iter{plan,build,total}, current_task,
  stall{count,n}, usage{waiting,reset_at,cycles_used}, halt{reason,task},
  last_checkpoint, last_session_id). KEY DESIGN: state.json is a CACHE not a store —
  missing/corrupt is a cache miss, not an error. Three load outcomes are
  distinguished so callers react precisely: missing → error wrapping
  `os.ErrNotExist`; present-but-unreadable (bad JSON OR unknown schema_version) →
  `*CorruptError`; other I/O error → returned verbatim. `LoadOrRebuild(path, store,
  fallbackPhase)` is the startup entry point: Load; on missing OR corrupt →
  `Rebuild` from the task store, returns `rebuilt=true`. `Save` is atomic
  (temp-in-same-dir + rename, mirrors `task.WriteFile`), MkdirAll's the parent, and
  stamps `UpdatedAt` to now on every call so "save on every transition" doubles as
  the TUI heartbeat. `reset_at` is `*time.Time` so null↔backoff round-trips.
  `Validate()` is STRUCTURAL only (schema==1, phase/run_state enum membership,
  non-negative counters) — it's the gate Load uses to detect corruption, so
  cross-field semantics (WAITING⇒usage.waiting) are left to Phase-3 transition
  helpers, not Validate. `Rebuild` derives the cursor from the ONLY ground-truth
  tier that exists today (the task store): prefers an `active` task (crash mid-loop)
  else `Next()`; leaves iter/stall.n/usage ZERO on purpose (config- and
  journal-derived — honest cache claims only what truth can prove). Wired into
  `src/cmd/flanders/main.go`: startup does `task.LoadDir(p.Tasks)` →
  `state.LoadOrRebuild(p.State, store, PhaseOrchestrate)` and logs run_state/phase/
  current_task. Verified: bare run logs `rebuilt=true phase=orchestrate
  run_state=RUNNING` (no state.json + no tasks dir = clean cache miss). 9 tests +
  full suite green.
  DEFERRED (documented, not stubbed): (a) the RUNNING-crash reconcile-against-git
  path (spec 09 §resume: re-read git status/diff to decide if an interrupted loop
  landed work) belongs to Phase 3.5 status-reconciliation — needs git, not built
  yet; (b) journal-tier rebuild enrichment (iter counts, last_session_id) lands with
  1.6; (c) Save-on-startup is intentionally NOT done — bare startup has no transition
  to persist, and persisting a rebuilt snapshot before the orchestrator fills
  config-derived fields would write a half-derived cache; the orchestrator (Phase 5)
  owns when to first persist. Schema-migration policy on a future `schema_version`
  bump = rebuild-from-truth (treat unknown version as corrupt) — OPEN in spec 09.)
- [x] **1.6 Journal writer** (`.flanders/journal/`, `01` §journal). Per-loop
  record: raw stream-json + summary (task, files touched, test result, cost,
  tokens, duration, session id). Append-only; readable back for the TUI history.
  *Acceptance:* a loop produces a re-readable journal entry.
  (Implemented in NEW package `src/lib/journal` (package `journal`), stdlib
  `encoding/json` only — NO new external dependency. TWO FILES PER LOOP keyed by
  an append-order seq: `<seq:06d>.json` (the `Summary` — task, files touched,
  test result, cost, tokens, duration, session id, status transition, subagents)
  + `<seq:06d>.stream.jsonl` (the verbatim raw NDJSON transcript). WHY two files:
  the spec says "raw stream-json PLUS a short summary"; the split lets the TUI
  history list render N tiny Summary parses without ever reading the (potentially
  huge) transcripts — drill-in loads one stream on demand.
  WRITE-ORDER INVARIANT: stream is written FIRST, summary LAST. The summary is
  the entry's commit marker (`List`/`Last`/`nextSeq` all key off `*.json`). A
  crash between writes leaves an orphan stream that `List` ignores and the next
  `Append`'s seq reuse overwrites — so failed writes neither leak seq numbers nor
  accumulate junk. Both writes are atomic (temp-in-same-dir + rename, same
  discipline as `state.Save`/`task.WriteFile`).
  JOURNAL OWNS THE SEQ (allocated as max-existing-filename + 1, caller's
  `Summary.Seq` is ignored/overwritten): keeps it a self-contained append-only
  log whose ordering can't be corrupted by a stale/duplicate caller index — the
  independence the spec-09 tier hierarchy needs (journal is tier 2; state.json is
  the tier-3 cache). Seq resumes across process restarts (a fresh `Open` of a
  populated dir continues numbering).
  RESILIENT `List()` skips unreadable/unparseable summaries (mirrors the stream
  parser's skip-bad-lines rule) so the TUI history renders even if one entry is
  damaged; only a dir-level glob failure errors. `Read(seq)` is strict — returns
  `*CorruptError` for a damaged entry, error wrapping `os.ErrNotExist` for a
  missing one.
  DECOUPLED from the not-yet-built stream-json parser (Phase 2.1): the journal is
  a PERSISTENCE concern only. `Summary` holds primitive fields the loop driver
  (Phase 3) fills from a future `LoopObservation`; `Append(s, raw io.Reader)`
  takes the raw transcript bytes to archive. So the on-disk record format has one
  owner (this package) and survives wire-protocol changes. Imports `src/lib/task`
  only, for the `Status`/`Reason` enums in the status-transition fields (single
  source of truth, no redefined strings).
  API: `Open(dir)`, `Append(*Summary, io.Reader) (seq, error)`, `List()
  ([]*Summary, error)` (seq-ordered), `Read(seq)`, `ReadStream(seq)
  (io.ReadCloser, error)`, `Last() (*Summary, error)` (nil,nil when empty),
  `Len()`. Helpers: `Tokens.Total()` (input+cache_read+cache_creation+output —
  matches spec-08 occupancy sum), `TestResult.Passed()` (Ran && exit 0; `Ran`
  distinguishes "tests not run this loop" from "ran and passed").
  WIRED into `src/cmd/flanders/main.go` startup: opens the journal after state
  load and logs `entries=<depth>` — the history depth the orchestrator will fold
  into a rebuilt `state.Iter`.
  10 tests + full suite green; `Version` const bumped to 0.0.6.)
- [ ] **1.7 Task-file model completeness (small gaps from audit).** Three minor
  spec-derived gaps in `src/lib/task`, none blocking but cheap to close: (a)
  **`id` ↔ filename-prefix validation** — spec 02 says `id` "matches filename
  prefix" but `LoadDir` (`store.go`) never checks that `0007-foo.md` carries
  `id: 0007`; add the check at load (warn or error — decide). (b) **`attempts`
  accessor/setter** — the `keyAttempts` constant exists (`task.go:69`) and the
  field round-trips via the yaml.Node, but there is no `Attempts()/SetAttempts()`;
  the build flow (4.4/4.5) needs to read+increment it for escalation. (c)
  **`SetNotes`/`SetFiles` setters** — getters exist, setters don't; the loop
  driver (3.5) and split pass (4.6) will need to write `notes`/`files`.
  *Acceptance:* id-mismatch surfaced at load; attempts round-trips through the
  typed API; notes/files writable. (Several sub-parts are spec-OPEN — gate against
  the consumers in Phase 3/4; do only the parts those consumers need.)

## Phase 2 — Agent integration & stream-json  `[depends: 1; highest technical risk]`

- [x] **2.1 Stream-json parser** in `src/lib` (`08-stream-json-protocol.md`).
  Streaming NDJSON decoder → typed events + a derived `LoopObservation` (tokens,
  cost, tool calls, subagent spawns, result/error, usage-limit + reset). Skip
  unparseable lines without crashing; preserve unknown types for the journal.
  *Acceptance:* fixture-based test over a captured real `claude 2.1.x` transcript
  asserts text/tool_use/result/token-usage extraction. **Capture a real transcript
  first** to pin wire shapes (spec 08 OPEN).
  (Implemented in NEW package `src/lib/stream` (package `stream`), stdlib only
  (`encoding/json`, `bufio`, `log/slog`) — NO new external dependency. Files:
  `stream.go` (typed events + Decoder), `observe.go` (`LoopObservation` aggregator
  + `Observe`/`ObserveFunc`), `stream_test.go` (9 tests), plus
  `testdata/basic.jsonl` (a tool-call transcript) and `testdata/subagent.jsonl`
  (a subagent-spawn transcript) — BOTH captured from REAL `claude 2.1.150` with
  `-p --output-format stream-json --verbose --include-partial-messages
  --dangerously-skip-permissions`. These fixtures ARE the contract (the spec-08
  acceptance gate).
  KEY DESIGN: line-oriented decoder (`bufio.Reader.ReadBytes`, no per-line length
  cap so big tool inputs/thinking signatures/file contents don't overflow) →
  `decodeLine` decodes a common envelope FIRST (`type`/`subtype`/`session_id`/
  `uuid`/`parent_tool_use_id` + `Raw` verbatim line) so an UNKNOWN top-level type
  still yields a usable `Event` with `Raw` preserved (forward-compatible); then a
  type-specific payload decode that is non-fatal (a payload mismatch logs but keeps
  the envelope+Raw, never loses a line). Unparseable lines are logged+skipped
  (`Decoder.Skipped` counter), never crash. `Decoder.Next()` is the pull
  primitive; `Stream(ctx, r, log)` is the channel wrapper the spec asked for.
  `Observe`/`ObserveFunc(r, log, onEvent)` folds the stream into a single
  `LoopObservation` (the spec's "one typed stream, no ad-hoc re-parsing");
  `ObserveFunc`'s per-event hook is the seam for the journal (raw archiving) and
  the Phase-2.2 live meter.
  `LoopObservation` extracts: `Texts`, `ToolCalls` (with `Parent` attribution +
  reconciled `tool_result` `IsError`/`Result`), `Subagents` (`Task`/`Agent`
  `tool_use` → `subagent_type`+`description`), `PeakLeadTokens` (LEAD-only context
  occupancy — subagent usage EXCLUDED on purpose per spec 01, tracked from
  `stream_event`/assistant `usage` where `parent_tool_use_id=="""`),
  `FinalUsage` (`result.usage` billing total incl. subagents), `Cost`,
  `ContextWindow` (from `result.modelUsage` — the CLI reports it), `Done`/
  `Subtype`/`IsError`/`ResultText`/`APIErrorStatus`, and usage-limit
  (`UsageLimited`/`ResetAt`/`RateLimitType` from `rate_limit_event`).
  `Occupancy(window)` helper falls back to the CLI-reported window when config
  window is 0.
  WIRE FINDINGS that extended the draft spec (all now PINNED into spec 08): a NEW
  `rate_limit_event` carries a clean epoch `resetsAt` (the usage-limit reset is
  NOT text-scraped — big de-risk for 2.3/3.12); `message_delta` carries FULL
  cumulative usage not just output_tokens; `result.modelUsage.<model>.contextWindow`
  (200000) means the CLI reports the window; the subagent-spawn tool is named
  `Agent` in 2.1.150 (parser accepts `Agent` OR `Task`); `parent_tool_use_id`
  attributes nested subagent activity. spec 08 has been updated from
  draft→PINNED. 9 tests, full suite + build + vet green.)
- [x] **2.2 Live token / context-occupancy tracker.** Fold `message_start` /
  `message_delta` usage into a running % of `[context].window_tokens`; expose for
  meters + guardrail. *Acceptance:* synthetic stream drives the % monotonically;
  trips at soft/hard. (`08` §live token tracking, `01` §context-pressure)
  (Implemented in NEW file `src/lib/stream/tracker.go` (`Tracker` type + `Trip`
  enum `TripNone|TripSoft|TripHard`) + `tracker_test.go` (6 tests). KEY REFACTOR:
  extracted `leadUsage(ev) (int,bool)` in `observe.go` as the SINGLE SOURCE OF
  TRUTH for "what counts as lead context" (lead-only — subagent
  `parent_tool_use_id` excluded; prefers `message_delta` cumulative usage over
  `message_start` via `StreamEvent.LiveUsage`; `Usage.Total()` over-counts cache
  categories so the guardrail trips early). Both the post-hoc `LoopObservation.fold`
  and the live `Tracker` now fold the SAME `leadUsage`, so they cannot drift —
  locked by `TestTrackerMatchesObservation` (Tracker peak == obs.PeakLeadTokens
  over the real `basic.jsonl` fixture). WHY a separate `Tracker` from
  `LoopObservation`: the observation is the post-hoc summary of a FINISHED loop;
  the Tracker is the IN-FLIGHT signal the guardrail reacts to mid-loop before any
  result event exists. Fed event-by-event via the existing `ObserveFunc`
  per-event hook (the seam — no new plumbing); MONOTONIC by construction (token
  high-water mark + window only resolves upward, so % and Trip never regress and a
  tripped tier stays tripped — the safe one-way guardrail decision). `Trip()`
  maps occupancy→tier at `soft_pct`/`hard_pct` (config `[context]`, defaults
  0.75/0.90 via `DefaultSoftPct`/`DefaultHardPct`); `SoftTripped()`/`HardTripped()`
  convenience. `Update` auto-adopts the CLI-reported window from
  `result.modelUsage.contextWindow` when `window_tokens=0`. Zero-value/no-window
  Tracker is INERT (`TripNone`, occupancy 0) — guarded so a bare struct can't
  spuriously hard-trip. NOT yet wired into a live loop (no loop driver until
  Phase 3); the guardrail CONSUMER is task 3.11, the process-supervisor seam is
  2.5. `go build/vet/test ./...` all green; `Version` bumped to 0.0.9.)
- [x] **2.3 Usage-limit detection + reset parse.** Classify an error `result` /
  non-zero exit as usage-limit vs. genuine error; extract `reset_at` (or fall back
  to `[usage].backoff`). *Acceptance:* known limit payloads → wait+reset; ordinary
  errors → error path. **Riskiest parse — verify wording vs 2.1.x.** (`08`, `01`)
  (Implemented in NEW file `src/lib/stream/classify.go` (`Outcome` enum
  `OutcomeSuccess|OutcomeUsageLimit|OutcomeError` + `String()`;
  `LoopObservation.Classify(exitCode int) Outcome`; `isUsageLimitResult`;
  `parseResetFromText`) + `classify_test.go` (9 tests). KEY DESIGN: `Classify` is
  the single 3-way decision the loop driver (3.1) and usage-wait guardrail (3.12)
  branch on, describing the INVOCATION outcome NOT task completion (done-ness stays
  the test gate's call). Usage-limit is checked FIRST and wins over the generic
  error path — misclassifying a limit as an error would abort an unattended
  multi-day run (spec 08). `UsageLimited` is now COMPREHENSIVE: still set from the
  out-of-band `rate_limit_event` (existing), and NOW ALSO from a usage-limit result
  — folded in `observe.go`'s `TypeResult` case via `isUsageLimitResult`
  (`api_error_status` containing 429, or result text matching `usage limit
  reached|usage limit exceeded|rate limit exceeded|rate_limit_error|too many
  requests`, case-insensitive). HTTP 529 (overloaded) is deliberately NOT a usage
  limit (transient server overload → error path). Reset extraction trust order:
  (1) `rate_limit_event` epoch, (2) `parseResetFromText` pulls a Unix epoch after
  the final `|` in the historical `Claude AI usage limit reached|<epoch>` message
  (ms epochs tolerated); the event epoch overrides a text-parsed one regardless of
  event order (fold only fills `ResetAt` from text when still nil, and the event
  path always overwrites). No backoff fallback in the stream package — that needs
  config, so the guardrail (3.12) applies `[usage].backoff` when `ResetAt` is nil.
  WHY synthetic test inputs not a `.jsonl` fixture: a real subscription limit could
  not be triggered to capture (spec 08 OPEN), so the limit transcripts are
  inline+labelled synthetic; the existing real `*.jsonl` fixtures (status
  `allowed`, success results) are unaffected (verified: their result text contains
  no limit phrase, `api_error_status` null). spec 08 updated: the classifier is now
  PINNED (best-effort heuristic, biased toward pausing per `max_cycles` cap) with a
  re-verify-against-real-limit note left in OPEN. Version bumped to 0.0.10; tag
  0.0.10. `go build/vet/test ./...` all green.)
- [x] **2.4 CLI invocation builder.** Compose `claude` args from config/phase: `-p`,
  `--output-format stream-json --verbose --include-partial-messages`, fresh
  `--session-id <uuid>` (no resume/continue), permission mode
  (`--dangerously-skip-permissions`, LOCKED default), `--model`/`--effort`
  per-phase, `--append-system-prompt` (rules), `--input-format stream-json` when
  `stream_input`. **No `--max-budget-usd` by default** (subscription). *Acceptance:*
  builder emits expected argv per phase/config. (`01` §invocation, `03`)
  (Implemented in NEW package `src/lib/invoke` (package `invoke`), stdlib only
  (`crypto/rand`, `fmt`) — NO new external dependency. Files: `invoke.go` (`Spec`,
  `Command`, `Build`, `NewSessionID`, `permissionArgs`) + `invoke_test.go` (11
  tests) + a `TestPhaseClass` added to `src/lib/config/config_test.go`. The builder
  is PURE: argv composition only, no process spawning (2.5) and no file I/O — the
  caller resolves paths and reads the rules file, then hands `Build` a `Spec`. WHY
  pure: makes the exact argv unit-testable (the acceptance, asserted by full-slice
  equality in `TestBuildDefaultPlan`) and gives the supervisor (2.5) one audited
  `Command{Bin,Args}` to launch (the shape `exec.CommandContext` wants).
  Pinned invariants (cited inline in the source): baseline `-p --output-format
  stream-json --verbose --include-partial-messages` ALWAYS emitted (required by
  `src/lib/stream`); fresh `--session-id <uuid>` every loop (NEVER
  `--resume`/`--continue`); permission mapping — `bypassPermissions` →
  `--dangerously-skip-permissions` (LOCKED default), the other three modes →
  `--permission-mode <mode>`; `--max-budget-usd` NEVER emitted (subscription, spec
  00/01) — guarded by `TestNeverEmitsBudget`; `--input-format stream-json` ONLY when
  `[agent].stream_input` (the soft-wind-down channel), and when on the prompt is
  delivered over stdin so it is NOT placed in argv (else a duplicate turn); when
  stream_input is off the prompt is the trailing positional to `claude -p`.
  `--model`/`--effort` per-phase from config (always emitted). `--effort` is the
  flag name (confirmed spec 07:15, NOT `--reasoning-effort`).
  KEY DECISION — phase→class resolution lives in `config.PhaseClass(phase)`, NOT in
  invoke: it is config INTERPRETATION (single source of truth); maps the four phases
  to their `[phases.*]` table and `split` reuses `[phases.plan]` (spec 07). Unknown
  phase = error (a typo surfaces at compose time, not as the wrong model silently).
  This is the phase HALF of task 4.1, landed early because 2.4 needs it; the
  SUBAGENT-class resolver (`[subagents]` default + per-class override merge) stays
  with 4.1 — the harness only ever invokes phase (lead) agents (subagents are
  spawned by the lead inside its own session), so the builder never needs a subagent
  class.
  `NewSessionID()` mints an RFC-4122 v4 UUID on `crypto/rand` (no external UUID
  dep); the CALLER owns the id (Build requires non-empty SessionID, never invents
  one) because it must also persist to journal/state for traceability (spec 01).
  Build errors loudly on inputs it alone owns: empty SessionID, unknown phase, nil
  config, unrecognized permission_mode. NOT yet wired into a live loop (loop driver
  is Phase 3); CONSUMERS are the supervisor 2.5 and iteration driver 3.1. `go
  build/vet/test ./...` all green. Version 0.0.11; tag 0.0.11.)
- [x] **2.5 Process supervisor.** Spawn/stream/wait the CLI; capture stdout(events)
  + stderr; enforce per-iteration timeout (kill); expose a writer for stream-json
  input injection (soft wind-down). *Acceptance:* runs a stub command, streams
  output, times out + kills cleanly.
  *Audit note — invoke contract:* when `[agent].stream_input=true`, `invoke.Build`
  deliberately omits the prompt from argv (it must go over stdin) with **no
  compile-time enforcement** (`invoke.go:117-121`). 2.5 owns that stdin write; make
  the supervisor's API make "you must write the prompt to stdin" un-missable (e.g.
  require a prompt-writer when the spec has `stream_input`), so a forgotten write
  can't send an empty turn.
  (Implemented in NEW package `src/lib/supervise` (package `supervise`), stdlib only (`os/exec`, `context`, `syscall`, `bufio`/`io`, `sync`) plus `src/lib/invoke` (for `Command`) and `src/lib/stream` (decode/fold). Files: `supervise.go` (`Spec`, `Result`, `Proc`, `Start`, `Run`, `Inject`, `CloseInput`, `Kill`, `Wait`), `proc_unix.go`/`proc_other.go` (build-tagged process-group kill), `supervise_test.go` (12 tests, stub-command based: cat over a stream-json fixture + `sh -c`, no real claude). API: `Run(ctx, Spec)` is the blocking convenience (spawn→stream→wait→Result); `Start` returns a live `*Proc` for the soft-wind-down guardrail (3.11) to `Inject`/`Kill` mid-loop. Folding stays internal via `stream.ObserveFunc` so the stream is decoded exactly once; `Result.Observation`+`Result.ExitCode` are exactly what `LoopObservation.Classify(exitCode)` consumes. KEY DECISIONS: (a) UN-MISSABLE STDIN CONTRACT — `Start` errors if `StreamInput && Prompt==""` and OWNS the initial prompt write to stdin, so invoke.Build dropping the prompt from argv (no compile-time enforcement) can never become an empty turn (closes the 2.5 audit note). (b) TIMEOUT/KILL via `exec.CommandContext`+`cmd.Cancel`+`cmd.WaitDelay`(5s): timeout creates a `context.WithTimeout` child; on expiry `cmd.Cancel` SIGKILLs the whole PROCESS GROUP (`Setpgid`, kill `-pgid`) so CLI-spawned subprocesses die too; `Result.TimedOut`/`Canceled` distinguish deadline vs parent-cancel (read before releasing the timer so DeadlineExceeded isn't masked). (c) RAW ARCHIVE — `Spec.RawSink` is teed off the SAME read the decoder consumes (`io.TeeReader`), so the journal archive is byte-faithful incl. skipped lines. (d) `Spec.OnEvent func(p *Proc, ev)` receives the `*Proc` (constructed before the read goroutine launches) so a guardrail can drive a `stream.Tracker` AND inject/kill without a closure-ordering race. (e) stderr → bounded 256KiB buffer (drops tail, never blocks the pipe). (f) `Wait` closes stdin FIRST then drains stdout/stderr before `cmd.Wait` (os/exec requires reads complete before Wait; closing stdin first avoids deadlock vs a read-to-EOF child like the `cat` stub). Verified green incl. `go test -race -count=2`. Acceptance MET: runs a stub command, streams output, times out + kills cleanly. NOT yet wired into a live loop (consumer = iteration driver 3.1 + context guardrail 3.11). Version 0.0.12; tag 0.0.12.)
- [ ] **2.6 Stream-json completeness pass (audit follow-ups).** Non-blocking
  decoder gaps found in the audit; close opportunistically as their consumers land:
  (a) **outbound stream-json envelope** — there is NO encoder for messages sent
  *into* the CLI; the soft-wind-down injection (3.11 tier 2) needs one. This is the
  one item with a hard downstream dependency (2.5 + 3.11) — do it with them. (b)
  **`rate_limit_event` epoch regression guard** — `observe.go:254-258` lets a later
  event with a *smaller* epoch overwrite a good `ResetAt`; keep the
  earliest/most-trustworthy. (c) **journal data-loss fields** — `ResultEvent` drops
  `duration_api_ms`/`ttft_ms`; `ModelUsage` drops `webSearchRequests`; the `user`
  wire's `tool_use_result` sibling (`stdout`/`stderr`/`interrupted`) and
  `content_block.caller` are kept only in `Raw`. Add the ones the journal/TUI
  actually render (esp. `tool_use_result` for LIVE-pane `✓/✗`, and `system`
  `task_started`/`task_progress`/`task_notification` subtypes for the AGENTS tree).
  (d) **real usage-limit fixture** — all limit tests are synthetic (spec 08 OPEN);
  capture a real limit transcript when one is available and re-verify `Classify`.
  *Acceptance:* outbound envelope round-trips; reset epoch never regresses;
  journal/TUI fields present where consumed.
  *(2.6a DONE: `stream.EncodeUserMessage(text)` added in NEW `src/lib/stream/encode.go` — the outbound user-turn NDJSON line (`{"type":"user","message":{"role":"user","content":[{"type":"text",...}]}}`) the soft wind-down (3.11) and discuss (7.x) inject; round-trips through the inbound Decoder (test-locked). Wire shape stays spec-08-OPEN/best-effort — re-verify vs a captured input transcript. 2.6b DONE: the `rate_limit_event` epoch regression is fixed in `observe.go` — `ResetAt` now only advances to a LATER epoch, never regresses to an earlier one (conservative usage-wait). REMAINING: 2.6c journal/TUI data-loss fields, 2.6d real usage-limit fixture.)*

## Phase 3 — The Ralph loop engine  `[depends: 1,2; core of the product]`

- [x] **3.1 Iteration driver** implementing the 8-step anatomy
  select→compose→spawn→observe→verify→evaluate→checkpoint→repeat (`01`).
  *Audit note — store hot-reload (spec 06 §refinement):* the driver MUST
  `task.LoadDir` (rebuild the store from disk) at the **top of each iteration**,
  not once at launch. An in-loop split (an agent writing new task files mid-loop)
  is otherwise invisible until restart. Single source of truth = the files on
  disk, re-read every loop.
  (DONE in NEW package `src/lib/loop`: `loop.go` = `Driver`/`Options`/`New`/`Result`/`Iterate`/`readRules`; `compose.go` = `composePrompt`/`planSummary`/`buildSummary`/`errorText`; `loop_test.go` = 7 tests. `Iterate` does select (store hot-reload via `task.LoadDir` at the top of EVERY call — the audit requirement above), compose (minimal — task file verbatim + one-line plan summary; 3.2 enriches), spawn (`invoke.Build`+`supervise.Run`, fresh session id, timeout + stream_input from config, raw transcript teed to journal via `RawSink`), observe (`Classify` + `journal.Append`). The driver READS task status before/after (reloading the file post-loop, since the agent edits its own status mid-run per spec 02) for the journal transition record but does NOT write status — reconcile/inference fallback is 3.5. state.json/iter/stall/usage stay the orchestrator's (Phase 5). An infra failure (load/build/spawn) errors; a loop that merely produced an error/limit/timeout RESULT returns normally with `Result.Outcome` set, since that is a routine guardrail-actionable outcome. NOT wired into `main.go` yet — Phase 5 calls Iterate. `Version` 0.0.14, tag 0.0.14.)
- [x] **3.2 Prompt composition (cost/quality lever).** Inject only: current task
  file + dependency outcomes + named spec excerpts + one-line done/left summary;
  rules via `--append-system-prompt`. Never the whole plan/journal. *Acceptance:*
  composed prompt contains the task + referenced excerpts and excludes unrelated
  tasks. (`01` §prompt composition)
  (DONE in `src/lib/loop/compose.go` — `composePrompt` is a `*Driver` method composing task file + dependency outcomes (`dependencyOutcomes`, frontmatter-derived summaries via `store.ByID`) + referenced spec excerpts (`specExcerpts`/`parseSpecRefs`/`readSpecSection`/`extractSection`, fence-aware, path-escape-guarded, skip-on-unresolvable) + one-line plan summary. Tests in `loop_test.go`. Version 0.0.17, tag 0.0.17.)
- [x] **3.3 Loop rules file** (`.flanders/rules.md`): one task/loop, flip own
  `status`, don't hand-edit harness state, delegate exploration to subagents,
  proactive context-overreach handoff. (`01`, `02`, `03` `rules_file`)
  (DONE — NEW package `src/lib/rules`: `DefaultMarkdown` (canonical rules text) + `WriteDefault` (atomic, no-overwrite). `flanders init` writes `.flanders/rules.md`; `loop.readRules` falls back to the embedded default when absent so rules are always applied. 5 rules tests + 2 loop readRules tests + extended init test. Version 0.0.20, tag 0.0.20.)
- [x] **3.4 Test gate (ground truth).** Run `[commands].test` (+ optional
  `build`/`lint`); exit 0 = pass. Harness-owned, not agent self-report.
  *Acceptance:* gate reflects real exit code. (`00` decision 2, `01` §done)
  (DONE in NEW package `src/lib/verify` — `verify.go` (`Kind`/`CommandResult`/`Result`/`Run`/`runOne`/`tail`) + `verify_test.go` (10 tests). Runs build→lint→test in declared order, each independently (NO fail-fast) so one pass records the full picture; `""`=skip (spec 03); `sh -c <cmdline>` in `paths.Root` preserves shell semantics. `CommandResult.Passed()`=ran+no-Err+exit-0; `Result.Passed()`=Test.Passed() (the spec's done-gate); `Result.OK()`=every-ran-command-passed. A non-zero exit is recorded in `ExitCode` (via `errors.As(*exec.ExitError)`), NOT returned as an error — "tests failed" must never look like "the harness broke"; a start/cancel failure sets `Err` + `ExitCode=-1`. Output capped to 64KiB tail. Wired into `loop.Driver.Iterate` step 5 via `runsTestGate(phase, outcome)`; result→journal `Summary.Test` + `loop.Result.Verify`. `go build/vet/test -race ./...` green; `Version` 0.0.15, tag 0.0.15.)
- [x] **3.5 Status reconciliation / inference fallback.** After a loop, infer
  outcome from `git diff` (work happened?) + test gate; when the harness itself
  ends a loop, **write `status`/`reason` directly**. *Acceptance:* outcome recorded
  whether or not the agent flipped status. (`02` §mutation ownership)
  *Audit note — reconciliation order (spec 01 OPEN):* make the precedence explicit
  — **check the agent-written `status` first**, fall back to git-diff + test-gate
  inference only when the agent left it unchanged. (Whether the agent flips status
  directly vs. the harness reconciles from a structured verdict is spec-OPEN; the
  fallback ordering above is the safe default.) This is also the home of the
  RUNNING-crash git-reconcile path that `state.LoadOrRebuild` defers (spec 09
  §resume): on resume of a RUNNING state, re-read `git status`/`git diff` to decide
  whether the interrupted loop actually landed work before continuing.
  (DONE — see PROGRESS 2026-05-26. NEW src/lib/git (read-only working-tree signal) + NEW src/lib/reconcile (agent-status-first, then test-gate promote, then active→pending normalize) wired into loop.Iterate step 6; journal Files populated from git diff; Result gains Reconcile/WorkHappened/FilesTouched. Agent-first precedence per the audit note. The deferred RUNNING-crash resume reconcile stays for Phase 5 (its consumer, the orchestrator, doesn't exist yet); the post-loop Reconcile it reuses is built/tested.)
- [x] **3.6 Git checkpointing.** Commit on progress (status change or passing
  tests); `commit_each` modes; `message_tmpl`; offer `git init` if target isn't a
  repo. *Acceptance:* progress commit created with templated message. (`01`, `03`)
  (DONE — see PROGRESS 2026-05-26. Write side added to `src/lib/git` (`Init`/`AddAll`/`Commit`/`Checkpoint`); `src/lib/loop/checkpoint.go` wires step 7 into `Iterate`; commit-on-progress with `commit_each` modes + `message_tmpl` ({phase}/{iter}/{task}/{result}); `init_if_missing` auto-inits a non-repo target; best-effort/never-fatal; new `Result.Checkpoint` sha for the orchestrator to persist as `state.last_checkpoint`. The interactive `git init` "offer" UX is a Phase-6/orchestrator concern — the engine uses the config flag as standing consent. NOTE: checkpoints commit the whole working tree; a target that does not gitignore `.flanders/` would commit harness journal/state too — acceptable for v1, revisit if it becomes noisy.)
- [x] **3.7 Done-detection.** Done iff test exits 0 **and** every task `done`
  **and** no stall. Agent report is advisory only. (`01` §done-detection)
  (DONE — `guardrail.Done(testPassed, allTasksDone, stalled bool) bool` in NEW package `src/lib/guardrail`. The locked spec-01 rule: all three conditions required; agent completion report is deliberately NOT a parameter.)
- [x] **3.8 Guardrail: max-iterations** per phase → halt + surface. (`01`,`03`)
  (DONE — `guardrail.MaxIterationsReached(iter, max int) bool`: trips when iter>=max (inclusive); max<=0 disables. The per-phase-vs-global apportionment is spec-06 OPEN and the orchestrator's to resolve (5.1) — this predicate only compares the counter it's given.)
- [x] **3.9 Guardrail: stall** — N consecutive no-file-change *and* no-status-change
  loops → halt. *Acceptance:* halts after `stall_n`. (`01`,`03`)
  *Audit note:* spell out the **reset** condition — the consecutive counter
  (`state.stall.count`) resets to 0 on any loop that produces a file change OR a
  status change; it only halts when it reaches `stall.n`. Test both the increment
  and the reset.
  (DONE — `guardrail.Changed(filesChanged, statusChanged bool) bool` (progress = touched files OR changed task status); `guardrail.StallStep(prev int, changed bool) int` (reset to 0 on progress, else prev+1 — both increment AND reset tested incl. end-to-end TestStallSequence); `guardrail.StallTripped(count, n int) bool` (trips when count>=n; n<=0 disables). Caller derives filesChanged from `loop.Result.WorkHappened` and statusChanged from `Result.Reconcile.From != Result.Reconcile.To`.)
- [ ] **3.10 Guardrail: per-iteration timeout** — kill + record. (uses 2.5)
- [x] **3.11 Guardrail: context-pressure (three-tier).** (a) proactive agent
  handoff (rule-driven); (b) **soft wind-down ~75%** via injected stream-json
  "wrap up" message when `stream_input`; (c) **hard kill ~90%** where the harness
  writes `blocked: context-overreach` + git-diff summary itself. Marker guaranteed
  all three ways. **Exhausted loop never splits itself.** *Acceptance:* each tier
  leaves a `blocked: context-overreach` task + handoff. (`01` §context-pressure,
  `06` §refinement)
  *Audit notes:* (1) **fallthrough when `stream_input=false`** — tier 2 (soft
  wind-down) is only available over the stdin stream-json channel; when it's off,
  go straight from tier 1 to tier 3 (hard kill) at the hard threshold. Call this
  path out explicitly. (2) Consumes `stream.Tracker` (`SoftTripped`/`HardTripped`,
  fed via the `ObserveFunc` per-event hook — seam confirmed) and the **outbound
  envelope from 2.6** for the injected message.
  (DONE — see PROGRESS 2026-05-26. NEW `src/lib/loop/context_pressure.go` + `task.SetNotes`; guard fed off the supervisor `OnEvent` hook drives a `stream.Tracker` and takes the three-tier action: tier-1 rule-driven, tier-2 soft wind-down injected via `Proc.Inject`/`EncodeUserMessage` when `stream_input` on, tier-3 hard kill where the harness writes `blocked: context-overreach` + handoff itself before reconcile. `stream_input=false` fallthrough tier1→tier3 handled. `Result.ContextTrip` surfaces the peak tier; journal Error names the backstop. 6 tests incl. a real-supervisor end-to-end over a 191k-token fixture. Version 0.0.21, tag 0.0.21.)
- [x] **3.12 Guardrail: usage-limit wait/auto-resume.** On limit (2.3): set
  `WAITING`, persist `reset_at`, sleep to reset (or `backoff`), auto-resume;
  honor `[usage].on_limit` (wait|halt) and `max_cycles`. State on disk ⇒
  close/reopen resumes. *Acceptance:* simulated limit → wait → resume; `halt` mode
  stops. (`01`, `09`)
  *Audit note — `max_cycles` accounting:* `state.usage.cycles_used` already exists
  in the schema; make the increment+cap explicit — bump `cycles_used` on each
  usage-wait resume and stop (per `on_limit`) when it reaches `[usage].max_cycles`
  (default unlimited). Use `ResetAt` when present, else `[usage].backoff` (the
  stream package deliberately leaves the backoff fallback to this consumer — it
  needs config).
  (DONE — NEW package `src/lib/usage`: pure `Decide` (on_limit + max_cycles cap) + `WaitDuration` (reset_at else backoff, past-reset → resume-immediately) + a `Waiter` with `HandleLimit` (fresh limit: persist WAITING+reset_at+incremented cycles_used BEFORE sleeping, then sleep-to-reset, then resume) and `Resume` (crash-resume: sleep remaining to reset_at, no re-bump). cycles_used counts windows entered; max_cycles=N drains N then halts; on_limit=halt halts immediately with st.Halt{reason,task}; cancelled ctx leaves WAITING on disk for restart. 18 tests, race-clean. NOT wired into main.go — Phase-5 orchestrator is the consumer. Version 0.0.22, tag 0.0.22.)

## Phase 4 — Phases & agent classes  `[depends: 3]`

- [x] **4.1 Agent-class resolution.** Map phase/subagent → model+effort from
  `[phases.*]`/`[subagents]` (+ overrides); `split` reuses `plan`. *Acceptance:*
  each class resolves to documented defaults unless overridden. (`07`,`03`) *(NOTE: the phase→class half already landed with task 2.4 as `config.PhaseClass`, incl. split→plan; 4.1 now covers only the subagent `[subagents]` default + per-class override merge.)* (DONE — `config.SubagentClass(name) AgentClass` in `src/lib/config/config.go`: global `[subagents]` default overlaid with any `[subagents.<name>]` override, merged field-by-field; unknown names resolve to the default (never error, since subagent names are open-ended). 2 tests. The phase→class half landed earlier with 2.4 as `config.PhaseClass`. Version 0.0.23, tag 0.0.23.)
- [x] **4.2 Plan loop.** Read `specs/*.md` (non-task) → create/update
  `specs/tasks/*.md`: decompose to smallest-checkable, assign ids, wire `deps`,
  write `acceptance`. *Acceptance:* a sample spec yields well-formed task files
  covering its requirements. (`02` §lifecycle)
  (DONE — NEW `src/lib/loop/plan.go`: `Driver.PlanIterate` (compose→spawn→observe→checkpoint), a SEPARATE method from `Iterate` (not task-selected, no verify/reconcile). `composePlanPrompt` = decompose instruction + existing-task summary (no bodies, store hot-reloaded) + non-task specs verbatim; no specs → error. Plan class opus/high, fresh session, `planWindDownMessage` for tier-2, hard kill ends loop (no task to block). Journal phase=plan/empty-task/no-gate; `planCheckpoint` commits task-file changes ({task}="(plan)" {result}="N tasks"). Shared helpers extracted: `Driver.commit` (from checkpoint), `Driver.baseSummary` (from buildSummary), `newContextGuard` (wind-down msg param). FIXED `git.WorkingChanges` to `--untracked-files=all` (per-file granularity for new untracked dirs). 5 tests; the Phase-5 orchestrator is the consumer that calls PlanIterate until plan-complete (4.3). Version 0.0.24, tag 0.0.24.)
- [x] **4.3 Plan-completeness check.** "Complete enough" = every spec requirement
  maps to ≥1 task (not provably perfect). *Acceptance:* uncovered requirement
  detected; covered plan passes. (`06` §plan-completeness — *judgment method is
  OPEN in spec; pick one*)
  *Audit note:* this check is the **plan-phase loop-exit condition** (step 6,
  "evaluate", of the 3.1 anatomy) — wire it as such, don't leave it standalone.
  Method is spec-OPEN (agent self-assessment vs. a mechanical coverage scan
  mapping each spec `## `/requirement to ≥1 task ref vs. — rejected — user
  approval); decide before implementing. Recommend the mechanical coverage scan
  (cheapest, harness-owned ground truth, parallels the test-gate philosophy).
  (DONE — NEW `src/lib/loop/plancheck.go`: `Driver.PlanComplete() *Coverage` runs the mechanical coverage scan (the chosen method; Finding 6). Requirements = `##` headings in non-task specs (fence-aware, reusing compose.go's `heading`/`isFence`); `###` sub-headings are aliases of the parent `##` (nested-credit); covered iff a task body references the spec by base name + heading/alias via `parseSpecRefs`+`headingMatches`. House-style meta (`OPEN`/index sections) excluded via `defaultExcludedSections` (grounded in spec 05 house-style) to keep the gate satisfiable. `Coverage{Specs,Tasks,Requirements,Covered,Uncovered}`+`Complete()`. Wired as the plan-loop exit signal via new `Result.PlanComplete` (best-effort, set after journal+checkpoint). 7 tests. The orchestrator (Phase 5) loops PlanIterate until Complete() and feeds Uncovered into the next re-plan. Version 0.0.25, tag 0.0.25.)
- [x] **4.4 TDD `test` agent loop (always-on).** For each task: ensure a **red**
  acceptance test exists — reuse if red; if a test already **passes** → mark task
  `done`, **skip build**; else write minimal red test. Author ≠ implementer.
  *Acceptance:* the three branches behave as specified. (`07` §test agent)
  *Audit note — spec-OPEN dependencies:* (1) how the test agent **locates** an
  existing test for a task (naming convention? filter? agent judgment?) is OPEN in
  spec 07 — decide before implementing. (2) `tdd=false` escape hatch is OPEN in
  spec 07 and has **no plan item**; if it's ever wanted, 4.4 needs a conditional
  bypass — track, don't build for v1.
  (DONE — see PROGRESS 2026-05-26. The test loop rides the `Iterate` spine (phase=="test"), not a separate method: `composePrompt` is phase-aware via `taskRoleHeader` (test-agent vs build-agent role), a green gate does NOT auto-promote in the test phase, and `classifyTestVerdict` (NEW `src/lib/loop/testphase.go`) sets `Result.TestVerdict` {Satisfied|RedReady|NoRedTest|Blocked|None} for the per-task build flow (4.5) to route on. Spec-07 OPEN "how the test agent locates an existing test" RESOLVED: agent status flip + whole-suite gate (per-task filtered gate = future refinement). An unconfirmed agent `done` is demoted to pending → build. 13 tests (8 new + 5 compose updated). The audit notes: (1) the locating-an-existing-test method is now decided (above); (2) `tdd=false` escape hatch remains OPEN/unbuilt for v1 as noted. Version 0.0.26, tag 0.0.26.)
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
  *Audit note — iteration-budget apportionment (spec 06 OPEN):* `[guardrails]
  .max_iterations` is a single config value, but plan loops and build loops both
  consume iterations. Decide whether the cap is per-phase or global (and how it's
  split) so a runaway plan loop can't exhaust the whole budget before build starts.
  `state.iter` already tracks `{plan,build,total}` separately — use those. Affects
  3.8 (the max-iter guardrail reads whichever scope is chosen).
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
  *Audit note — config prerequisite:* the `Config` struct has **no `[tui]`
  section** today (confirmed in `src/lib/config/config.go`). Add a `TUI`/`Theme`
  section (per-role override keys are spec-OPEN — define them here) to
  `src/lib/config` before 6.1 can honor `[tui].theme`. Small, but a hard
  prerequisite.
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
  *Audit note — sequencing:* this has **no Bubble Tea dependency** (it's a pure
  task-files → markdown renderer, spec 02 infrastructure). It is filed under
  Phase 6 but could land as early as Phase 4 (once 4.2 produces task files),
  giving a useful artifact much sooner. Consider pulling it forward.
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
  *Audit note:* pin down the `p` key's exact behavior (spec 05 says it "triggers"
  plan; 7.4 says "suggest") — recommend: `p` exits discuss and launches
  `flanders plan` (the human pressing it IS the control point), distinct from the
  passive on-exit text suggestion.
- [ ] **7.5 Discuss agent system prompt + `specs/`-scope enforcement** `[AUDIT GAP
  — no prior plan item]`. Spec 05 requires (a) a discuss-specific **system/role
  prompt** (drive to decisions, surface trade-offs, ask focused questions, write
  decisions to disk *as made*; maintain spec house-style; **user owns
  granularity** — propose, don't impose detail) — the discuss analogue of the 3.3
  loop rules file, which currently has no item; and (b) **technical enforcement
  that Write/Edit is scoped to `specs/`** plus read-only exploration of the target
  codebase, under `bypassPermissions`. Today the only scoping mechanism in the
  design is prompt rules — decide whether prompt-level scoping suffices or a real
  guard is needed. (`config.PhaseClass("discuss")` already resolves opus/high — the
  model side is done.) *Acceptance:* discuss agent boots with its role prompt and
  cannot write outside `specs/`.
- [ ] **7.6 Discuss session lifecycle + spec-OPEN behaviors** `[AUDIT GAP]`.
  (a) **Long-lived per-turn loop** — discuss is the ONLY non-Ralph mode: it keeps
  one `claude` session and injects each user turn over stdin stream-json (no fresh
  session per turn). Model this explicitly (it's the opposite of the loop engine's
  fresh-context-every-iteration). (b) **Context-exhaustion behavior** — the Ralph
  context guardrails (3.11) are loop-specific and don't apply; spec 05 leaves
  discuss-window overflow undefined. Decide (e.g. summarize-and-continue, or warn
  and require restart). (c) Two spec-05 **OPEN** items with no coverage: a
  pre-handoff *readiness check* ("no blocking `OPEN`s remain?"), and whether
  discuss can be **re-entered during a paused build** to amend specs then resume.
  Track; resolve in a discuss-spec pass before building 7.x.

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
   (RESOLVED/PINNED: wire shapes verified against `claude 2.1.150` with captured
   fixtures in `src/lib/stream/testdata/`; spec 08 updated from draft→PINNED; task
   2.1 DONE). The riskiest-parse concern (usage-limit detection) is structurally
   de-risked: `rate_limit_event` carries a clean epoch `resetsAt` — no text-
   scraping required. The exact exhausted-status string (how to distinguish a
   usage-limit `result` from a genuine error) is now DONE in task **2.3**: the
   classifier is implemented and spec 08 PINNED as a best-effort heuristic (matches
   known phrasings + API 429, biased toward pausing), with the exact exhausted
   wording still to be re-verified against a real captured limit. Remaining risk:
   task **2.5** (process supervisor; the CLI invocation builder 2.4 is now DONE in `src/lib/invoke`).
2. **`state.json`** → authored `specs/09-state-and-resume.md` (draft) and IMPLEMENTED
   in `src/lib/state` (task 1.5 done). Persistence + recovery (missing/corrupt →
   rebuild from task store) are complete; the RUNNING-crash git-reconcile path is
   deferred to Phase 3.5. Journal-tier rebuild (task 1.6) is NOW DONE: the
   `src/lib/journal` package exists and exposes `Last()`/`Len()`/`List()` as the
   seam for journal-tier state rebuild; the actual enrichment of
   `state.Iter`/`last_session_id` from the journal still belongs to the
   orchestrator (Phase 5), since `state.Rebuild` itself stays ground-truth-only.
   The usage-wait/resume *consumer* of this state is still task **3.12**.
3. **`flanders init` inconsistency** — RESOLVED. `init` was referenced in
   `03-config.md` ("missing → `flanders init` …") but absent from the command
   surface in `00-overview.md`. Now resolved: `init` has been added to the command
   surface in `specs/00-overview.md`, and `specs/03-config.md` has been clarified
   to make the starter-vs-overlay-default `[commands]` nuance explicit (the
   `DefaultTOML` template writes Go starters `test`/`build`; `config.Default()` is
   unchanged and stack-agnostic). Task **1.2** is complete.
4. **Operator-notes file undefined** — `04-tui.md` `i` writes "an operator-notes
   file the harness folds into that loop's prompt," but no path/format in
   `03-config.md`. Define it (task 8.2) — candidate `[paths].notes`.
5. **Model→context-window table is OPEN** (`03-config.md`) but **required** to turn
   token counts into a % when `window_tokens = 0` (task 2.2). SUBSTANTIALLY
   ANSWERED: the CLI reports `result.modelUsage.<model>.contextWindow` (e.g.
   200000) at result time, surfaced as `LoopObservation.ContextWindow` — so the
   window is available after the first completed loop without any hardcoded table.
   RESOLVED (decided in 2.2): NO static model→window map is shipped. The live
   meter relies on config `[context].window_tokens` (default 200000) as the seed
   before the first result, and `stream.Tracker.Update` auto-adopts the
   CLI-reported window (`result.modelUsage.contextWindow`) at result time to
   confirm/correct it for subsequent loops. So `window_tokens` is the live seed
   and the CLI is the source of truth thereafter — no hardcoded table. (Loader
   side ready: `config` accepts `window_tokens = 0` as the "auto-detect" sentinel;
   `Occupancy(window)` in `src/lib/stream` falls back to the CLI-reported window
   when config window is 0.)
6. **Plan-completeness method** (`06`) — RESOLVED (task 4.3 DONE): chose the **mechanical coverage scan** (`src/lib/loop/plancheck.go` `Driver.PlanComplete`) over agent self-assessment / (rejected) user approval — harness-owned ground truth, same philosophy as the test gate. Requirements = `##` headings (fence-aware, `###` as aliases), house-style meta (`OPEN`/index) excluded per spec 05. Spec 02 §Plan lifecycle and spec 06 §Plan-completeness criterion both note the method was OPEN; consider a discuss-pass to pin "coverage check" into those specs.
7. **Stale spec note** — multiple specs say "the harness's own directory is not yet
   a git repo"; it now **is** (branch `main`, one commit). Minor cleanup for a
   future discuss pass; doesn't block anything.
8. **OPEN items that don't block a first build** (track, don't gate): permission
   mode default (locked to bypass anyway), guardrail recovery UX (`01`/`06`),
   single-screen vs full-screen LIVE (`04`), `--no-tui` log format, `tdd=false`
   escape hatch (`07`), test-command auto-detect (`03`), optional task frontmatter
   `attempts` (`02`).

### Audit re-run 2026-05-25 — new confirmed findings

9. **`[paths]` config is a silent no-op** `[RESOLVED — task 0.5]`.
   `paths.New(root)` (`src/lib/paths/paths.go:48-66`) always uses the hardcoded
   `Default*` constants; nothing overlays `config.Paths` (parsed + validated by
   `src/lib/config`). A user's `[paths]` section is therefore ignored — spec-03
   non-compliance. → **task 0.5** (highest-priority correction; cheap). **FIXED by 0.5**:
   `paths.NewFromConfig` overlays the configured `[paths]` (and also fixed the parallel
   `[agent].rules_file` no-op, which had the same bug), and `runOrchestrate` loads the
   config at startup.
10. **Stream decoder drops spec-documented fields** (all non-blocking, → task 2.6):
    `ResultEvent` lacks `duration_api_ms`/`ttft_ms`; `ModelUsage` lacks
    `webSearchRequests`; the `user` wire's `tool_use_result` sibling
    (`stdout`/`stderr`/`interrupted`), `content_block.caller`, the `cache_creation`
    sub-object, and `system` `task_started`/`task_progress`/`task_notification`
    subtypes survive only in `Raw`. The TUI (LIVE `✓/✗`, AGENTS tree) and journal
    will want several of these. **`rate_limit_event` epoch regression: FIXED (2.6b)** — `ResetAt` in `observe.go` now only advances to a later epoch, never regresses to an earlier one (conservative usage-wait). Remaining gaps → task 2.6(c)(d).
11. **Outbound stream-json encoder: NOW EXISTS (2.6a DONE).** `stream.EncodeUserMessage` (`src/lib/stream/encode.go`) produces the outbound user-turn NDJSON line; round-trips through the inbound Decoder (test-locked). **Supervisor un-missable stdin contract: IMPLEMENTED (2.5 DONE)** — `supervise.Start` errors if `StreamInput && Prompt==""` and owns the initial prompt write to stdin, so the missing-write bug is caught at call time. The 2.6a `EncodeUserMessage` envelope is now CONSUMED live: `supervise.Proc.Inject` encodes the text with it, and 3.11's tier-2 soft wind-down calls `Inject` — so the outbound envelope is exercised end-to-end (test-locked via the guard's inject-once tests). No remaining 2.6 hard-dependency for 3.11.
12. **Loop-engine scoping requirements the items hadn't spelled out** (now folded
    into 3.x): per-iteration **store hot-reload** for in-loop splits (→3.1);
    **stall-counter reset** semantics (→3.9); **`max_cycles`/`cycles_used`**
    increment+cap (→3.12); **soft-wind-down fallthrough** to hard-kill when
    `stream_input=false` (→3.11); **reconciliation order** agent-status-first then
    git-diff inference, incl. the deferred RUNNING-crash git-reconcile (→3.5) [reconciliation order + post-loop reconcile DONE in 3.5 via src/lib/reconcile; RUNNING-crash resume reconcile deferred to Phase 5];
    **iteration-budget apportionment** plan-vs-build, spec-06 OPEN (→5.1/3.8).
13. **Discuss mode has thin plan coverage** (new tasks 7.5/7.6): no item for the
    discuss agent's **system/role prompt**, none for **`specs/`-scope write
    enforcement**, none for the **long-lived per-turn injection loop**, and three
    spec-05 OPENs uncovered (discuss context-exhaustion, pre-handoff readiness
    check, re-enter-discuss-during-paused-build).
14. **`[tui]` config section absent** `[CONFIRMED]`. `Config` has no `TUI`/theme
    section, yet 6.1 promises `[tui].theme` overrides — add it to `src/lib/config`
    before 6.1 (→ noted on 6.1). Per-role override keys remain spec-OPEN.
15. **Logging: no `[logging]` section / level not config-wired; no rotation.** The
    `logging.ParseLevel` helper exists but nothing maps a config field to it (level
    is hardcoded at `runOrchestrate`). **The level-not-config-wired part is now a
    DELIBERATE deferral decided in 0.5**: spec 03 has no `[logging]` section, so wiring
    a level field would be speculative spec-extension — left hardcoded (`slog.LevelInfo`)
    until a `[logging]` section is specced; the concern is documented at the logger-init
    code site. Rotation is deferred-by-design (task 0.3
    note) — fine for now, revisit for multi-day unattended runs. Minor; fold into
    the `[tui]` config-section work or a later config pass.
16. **Small task-model gaps** (→ task 1.7): `id`↔filename-prefix not validated at
    load; `attempts` has no typed accessor/setter; no `SetNotes`/`SetFiles`. All
    minor / partly spec-OPEN; close against the Phase-3/4 consumers that need them.
17. **Stale code comment** `[trivial]`. `src/lib/state/state.go:250-252` still says
    "the journal is tier 2, **not built yet** — 1.6"; the `src/lib/journal` package
    now exists (1.6 done). `state.Rebuild` correctly still does NO journal
    enrichment (that's the orchestrator's job, Phase 5), so this is a comment-only
    cleanup — fix it when the orchestrator wires journal-tier rebuild in Phase 5,
    not before.

## Build conventions

- Module `flanders`; packages under `src/`, imported as `flanders/src/lib/<pkg>`. Build/test/vet/run commands are in AGENTS.md. First green tag: `0.0.1`.
- External dependencies: `github.com/BurntSushi/toml v1.4.0` (config parsing, TOML);
  `gopkg.in/yaml.v3 v3.0.1` (task-file frontmatter, YAML — node-based for lossless
  round-trip). Config is TOML, task files are YAML — both by spec design. State
  (`src/lib/state`) and journal (`src/lib/journal`) both use stdlib `encoding/json`
  only — no new dependency. `src/lib/git` (shells to the git binary, stdlib os/exec)
  and `src/lib/reconcile` (pure, imports task) add no external dependency. `src/lib/guardrail` (pure, imports nothing) adds no external dependency. `src/lib/usage` (imports config + state, no external dependency) adds no external dependency.

## Working agreements (from PROMPTs)

- Search before assuming missing; **don't reimplement** — consolidate in `src/lib`.
- One unit of work per loop; run the **relevant** tests after each change; keep
  this file current via a subagent; commit + push + tag on green.
- Single source of truth, **no migrations/adapters**; fix unrelated failing tests
  as part of the increment; finish features (no stubs/placeholders).
