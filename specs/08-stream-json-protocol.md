# Flanders — Stream-JSON Protocol

> Status: **draft for review**. The *requirements* (what the harness must extract
> and which feature each field drives) are decided; the exact **wire shapes**
> below are inferred for `claude` CLI **2.1.x** and must be **pinned against the
> real CLI during implementation** — every wire detail is `OPEN` until verified.
> Authored because specs `01`/`04` repeatedly say "parse stream-json" without
> defining the contract the whole observability + guardrail layer depends on.

**Keywords:** stream-json · protocol · events · ndjson · parser · partial-messages
· tool_use · subagent-spawn · token-usage · context-pressure · cost · result-event
· usage-limit · reset-time · rate-limit · session-id · bidirectional · input-format

## Why this spec exists

Almost every observable behavior in Flanders is derived from the `claude` CLI's
event stream, not from the agent's prose:

- the **LIVE** pane and **AGENTS** tree (`04-tui.md`) render parsed events;
- the **journal** (`01-ralph-loop.md`) stores the raw stream plus a summary;
- the **context-pressure guardrail** (`01`) trips on *live token usage* read from
  the stream — so the token fields are load-bearing, not cosmetic;
- the **cost meter** (info-only) reads `total_cost_usd`;
- **usage-limit auto-resume** (`01`) parses a **reset time** out of an error result.

If the parser is wrong, the TUI lies and the guardrails mis-fire. Hence a
dedicated contract.

## Transport

- Invocation (from `01-ralph-loop.md` §Agent invocation):
  `claude -p --output-format stream-json --verbose --include-partial-messages`.
- Output is **newline-delimited JSON** (one JSON object per line) on stdout. The
  parser reads line-by-line; a line that fails to parse is logged and skipped
  (never crashes the loop).
- For the soft wind-down and `discuss`, input is **also** stream-json
  (`--input-format stream-json`), so the harness can inject a user message into a
  running session (`03-config.md` `stream_input = true`).

## Event types the harness consumes (assumed shapes — verify vs 2.1.x)

Top-level objects carry a `type`. The harness cares about:

| `type` | Carries | Harness uses it for |
|---|---|---|
| `system` (`subtype:"init"`) | `session_id`, `model`, `tools`, `cwd` | confirm session start; record session id in journal |
| `assistant` | `message` = Anthropic Messages object (content blocks) | LIVE pane: `💬` text, `⏺` tool calls; detect `Task` tool → subagent spawn |
| `user` | `message` (tool_result blocks) | LIVE pane: tool result ✓/✗ |
| `stream_event` | raw Anthropic streaming `event` (only with `--include-partial-messages`) | **live token tracking** (see below); incremental text for responsive LIVE pane |
| `result` | terminal event (see below) | done/cost/usage/error + **usage-limit reset** |

### Content blocks (inside `assistant.message.content`)

- `text` → assistant prose (`💬`).
- `tool_use` → `{name, input, id}`. Render as `⏺ <name> <terse args>`. A
  `tool_use` whose `name` is the subagent/Task tool (`Task`/`Agent`) is a
  **subagent spawn** → add a node to the AGENTS tree (`07-agents-and-models.md`),
  labelled with the requested model/effort when present in `input`.
- (in `user.message.content`) `tool_result` → `{tool_use_id, content, is_error}`
  → render ✓ (ok) or ✗ (`is_error`).

### Live token tracking (the context-pressure input)

Token counts arrive on streaming sub-events (`type:"stream_event"`):

- `message_start` → `event.message.usage` with `input_tokens`,
  `cache_read_input_tokens`, `cache_creation_input_tokens`.
- `message_delta` → `event.usage.output_tokens` (cumulative for the turn).

The harness maintains a running **context-occupancy estimate** =
(input + cache_read + cache_creation + output) for the live turn, divided by
`[context].window_tokens` (`03-config.md`). This % drives the meter color
transitions and the soft (`75%`) / hard (`90%`) context-pressure trips
(`01-ralph-loop.md` §Guardrails). **OPEN:** confirm which usage fields the 2.1.x
stream actually emits and how cache tokens count toward the window; pick the
estimator accordingly (err on the side of over-counting so trips fire early).

### The `result` event (terminal, once per session)

Assumed fields:

- `subtype` — `success` | `error_max_turns` | `error_during_execution` | … .
- `is_error` (bool), `result` (final text or error message).
- `total_cost_usd`, `usage` (final token totals), `num_turns`, `duration_ms`,
  `session_id`.

Drives: journal summary (cost/tokens/duration), and — together with a non-zero
process exit — **error classification** below.

## Usage-limit / rate-limit detection (load-bearing for auto-resume)

On a subscription rate/usage exhaustion (`00-overview.md`, `01` §Guardrails) the
harness must (a) recognize it and (b) extract a **reset time** to sleep until.

- **Signal:** an error `result` (e.g. `subtype` indicating a limit, or
  `is_error:true` with a rate-limit message) and/or a non-zero process exit.
- **Reset time:** parsed from the error payload (`result` text and/or stderr).
  Expected forms to match: an explicit reset timestamp/epoch, or a human phrase
  ("resets at 3pm", "try again in N minutes"). **OPEN — must verify the actual
  2.1.x wording/field**; this is the riskiest parse in the system.
- **Fallback:** if no reset time is parseable, wait `[usage].backoff`
  (`03-config.md`).
- **Distinguish** usage-limit (→ *wait/auto-resume*, not a failure) from genuine
  agent errors (→ guardrail/halt path). Misclassifying a limit as an error would
  abort an unattended multi-day run.

## Parser placement & shape

- Lives in `src/lib` (project standard library) as a reusable, streaming decoder:
  raw `io.Reader` → typed Go event values on a channel, plus a derived
  `LoopObservation` (tokens, cost, tool calls, subagent spawns, result/error,
  usage-limit + reset). One source of truth; the TUI, journal, and guardrails all
  consume the same typed stream (no ad-hoc re-parsing per consumer).
- Unknown `type`s and unknown content blocks are preserved verbatim for the
  journal but ignored by feature logic (forward-compatible with CLI changes).

## OPEN

- **Pin every wire shape against `claude` 2.1.x** (capture a real
  `--output-format stream-json --verbose --include-partial-messages` transcript
  and assert field names/paths in a fixture-based parser test).
- Exact usage-limit error `subtype`/field and reset-time wording.
- Whether cache tokens count toward the context window for the pressure estimate.
- Subagent model/effort discoverability from the `Task` tool input vs. inferring
  from `[subagents]` config.
- Outbound (input-format stream-json) message envelope for the soft wind-down /
  discuss injection.
