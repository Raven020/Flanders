# Flanders — Stream-JSON Protocol

> Status: **wire shapes PINNED** against `claude` CLI **2.1.150**, captured with
> the exact flags the harness uses
> (`claude -p --output-format stream-json --verbose --include-partial-messages
> --dangerously-skip-permissions`) over **two** real transcripts, and asserted by
> a fixture-based parser test (`src/lib/stream`, fixtures in
> `src/lib/stream/testdata/`: `basic.jsonl`, `subagent.jsonl`). The *requirements*
> (what the harness must extract and which feature each field drives) were already
> decided; the **inbound** wire shapes below are now ground truth. A handful of
> items remain `OPEN` and are marked as such — notably the exact `status` wording
> when a usage window is exhausted, and the **outbound** (input-format) envelope,
> which we have **not** yet captured.
> Authored because specs `01`/`04` repeatedly say "parse stream-json" without
> defining the contract the whole observability + guardrail layer depends on.

**Keywords:** stream-json · protocol · events · ndjson · parser · partial-messages
· tool_use · subagent-spawn · token-usage · context-pressure · cost · result-event
· usage-limit · reset-time · rate-limit · rate-limit-event · resetsAt · context-window
· modelUsage · parent-tool-use-id · subagent-type · session-id · bidirectional · input-format

## Why this spec exists

Almost every observable behavior in Flanders is derived from the `claude` CLI's
event stream, not from the agent's prose:

- the **LIVE** pane and **AGENTS** tree (`04-tui.md`) render parsed events;
- the **journal** (`01-ralph-loop.md`) stores the raw stream plus a summary;
- the **context-pressure guardrail** (`01`) trips on *live token usage* read from
  the stream — so the token fields are load-bearing, not cosmetic;
- the **cost meter** (info-only) reads `total_cost_usd`;
- **usage-limit auto-resume** (`01`) reads a **reset time** — now from a clean
  `rate_limit_event` epoch, not text-scraped from an error (see below).

If the parser is wrong, the TUI lies and the guardrails mis-fire. Hence a
dedicated contract.

## Transport

- Invocation (from `01-ralph-loop.md` §Agent invocation):
  `claude -p --output-format stream-json --verbose --include-partial-messages`
  (loops add `--dangerously-skip-permissions`; see `03-config.md`).
- Output is **newline-delimited JSON** (one JSON object per line) on stdout. The
  parser reads line-by-line; a line that fails to parse is logged and skipped
  (never crashes the loop). It imposes no per-line length limit — tool inputs,
  thinking signatures, and file contents can be large.
- **Common envelope:** every line carries `type`, `uuid`, and `session_id`.
  Nested **subagent** activity additionally carries a non-empty
  `parent_tool_use_id` (see §Subagents). This envelope is decoded first, so an
  unknown `type` still yields a usable event with its raw bytes preserved.
- For the soft wind-down and `discuss`, input is **also** stream-json
  (`--input-format stream-json`), so the harness can inject a user message into a
  running session (`03-config.md` `stream_input = true`). **OPEN:** the outbound
  envelope shape — we have not captured an input transcript yet (see §OPEN).

## Event types the harness consumes (verified against 2.1.150)

Top-level objects carry a `type`. Six types were observed; the harness cares
about all of them. The `rate_limit_event` type is **new** versus the original
draft, which had assumed usage-limit info would only appear inside an error.

| `type` | Carries | Harness uses it for |
|---|---|---|
| `system` (`subtype:"init"` \| `"status"`) | init: `session_id`, `model`, `permissionMode`, `tools[]`, `cwd` (+ more) | confirm session start; record session id in journal |
| `stream_event` | a raw Anthropic streaming `event` (only with `--include-partial-messages`) | **live token tracking**; incremental text for a responsive LIVE pane |
| `assistant` | `message` = assembled Anthropic Messages object (complete content blocks) | LIVE pane: `💬` text, `⏺` tool calls; detect subagent-spawn tool → AGENTS tree |
| `user` | `message` (tool_result blocks) | LIVE pane: tool result ✓/✗ |
| `result` | terminal event (see below) | done/cost/usage/error + **model context window** |
| `rate_limit_event` | `rate_limit_info` (usage-window state, out-of-band) | **usage-limit auto-resume**: clean epoch reset time |

### `system`

Two subtypes observed:

- `init` — **full**: `session_id`, `model` (observed `claude-sonnet-4-6`),
  `permissionMode` (observed `bypassPermissions`), `tools[]`, `cwd`, plus many
  more keys. The harness reads model/permission/tools to confirm the session
  started as configured and to seed the journal header.
- `status` — **sparse** heartbeat-ish line; preserved verbatim, no feature logic
  depends on it today.

### `stream_event` — raw Anthropic streaming events (the live-token source)

`stream_event` wraps a raw Anthropic streaming event under `.event`. Observed
sub-types (`.event.type`): `message_start`, `content_block_start`,
`content_block_delta`, `content_block_stop`, `message_delta`, `message_stop`.

- `message_start` → usage at **`.event.message.usage`** =
  `{input_tokens, cache_creation_input_tokens, cache_read_input_tokens,
  output_tokens, cache_creation:{ephemeral_5m_input_tokens,
  ephemeral_1h_input_tokens}, service_tier, …}`.
- `message_delta` → the **FULL cumulative usage** at **`.event.usage`**
  (`input_tokens, cache_creation_input_tokens, cache_read_input_tokens,
  output_tokens`, plus an `iterations` array) — **not** just `output_tokens` as
  the draft assumed. `.event.delta` carries `{stop_reason, stop_sequence,
  stop_details}`.
- `content_block_start` → `.event.content_block` types observed: `thinking`,
  `tool_use` (`{id, name, input, caller:{type:"direct"}}`), `text`.
- `content_block_delta` → `.event.delta` types: `text_delta` (`.text`),
  `thinking_delta` (`.thinking`), `signature_delta` (`.signature`),
  `input_json_delta` (`.partial_json`, the tool_use args streamed in).

**Why it matters:** these sub-events are the only *live* (mid-turn) token signal,
so they drive the context-pressure guardrail in real time. Because
`message_delta` carries the full cumulative usage object, the harness has
input + cache + output for the turn at every delta — it does not have to stitch
`message_start` input to a running output count. Complete text/tool_use blocks
are read off the assembled `assistant` messages instead (below), which is far
simpler than reassembling deltas.

### Live token tracking (the context-pressure input)

The harness maintains a running **context-occupancy estimate** for the live turn:

    input_tokens + cache_read_input_tokens + cache_creation_input_tokens + output_tokens

divided by the window (see §Context window). This % drives the meter color
transitions and the soft (`75%`) / hard (`90%`) context-pressure trips
(`01-ralph-loop.md` §Guardrails). The most complete usage object available is
preferred: `message_delta`'s `.event.usage` when present, else
`message_start`'s `.event.message.usage`.

**Decision (was OPEN):** **over-count** — include the cache token categories in
the estimate. Erring high makes the guardrail trip *early* rather than late,
which is the safe failure mode for an unattended run. **Still OPEN:** whether
cache tokens *truly* occupy the model's usable window the same way fresh input
does; the over-count decision stands regardless, but the precise accounting is
unconfirmed.

### `assistant` — assembled complete messages

`assistant` events are the **assembled** complete messages (full `tool_use`
`input` present), which is why the harness reads them for display rather than
reassembling deltas. Shape:

    {type:"assistant",
     message:{role, model, content[], stop_reason, usage, …},
     parent_tool_use_id, session_id}

Content blocks observed:

- `text` → `{type, text}` → assistant prose (`💬`).
- `thinking` → `{type, thinking}` → reasoning (rendered/dimmed per `04-tui.md`).
- `tool_use` → `{type, id, name, input}`. Render as `⏺ <name> <terse args>`. A
  `tool_use` whose `name` is the subagent tool is a **subagent spawn** (§Subagents).

### `user` — tool results

`user.message.content` is a list of `tool_result` blocks:
`[{tool_use_id, type:"tool_result", content, is_error}]`. **`content` is either a
bare string OR an array of `{type, text}` parts** — the parser handles both.
There is also a sibling top-level `tool_use_result` object with
`{interrupted, isImage, noOutputExpected, stderr, stdout}` (the structured
command result). Rendered ✓ (ok) / ✗ (`is_error`) in the LIVE pane.

### The `result` event (terminal, once per session)

Far richer than the draft assumed. Observed keys:

- `subtype` — observed `"success"`; error forms expected
  `error_max_turns` | `error_during_execution` | … (`OPEN` exact set).
- `is_error` (bool), and `api_error_status` — **`null` on success, non-null on an
  API-level error** (a distinct error signal from `is_error`).
- `result` (final assistant text), `stop_reason`, `terminal_reason`
  (observed `"completed"`).
- `total_cost_usd`, `duration_ms`, `duration_api_ms`, `ttft_ms`, `num_turns`,
  `session_id`.
- `usage` = `{input_tokens, cache_creation_input_tokens, cache_read_input_tokens,
  output_tokens, server_tool_use, iterations, …}` — final totals.
- **`modelUsage`** = a map of `model-id` → `{inputTokens, outputTokens,
  cacheReadInputTokens, cacheCreationInputTokens, costUSD, contextWindow,
  maxOutputTokens}`.
- `permission_denials[]`.

Drives: journal summary (cost/tokens/duration), error classification (below), and
— via `modelUsage` — the model's context window (next section).

### Context window — the CLI reports it (`modelUsage.<model>.contextWindow`)

`modelUsage.<model>.contextWindow` (observed **200000**) means **the CLI itself
reports the model's context-window size**. This substantially answers the OPEN
model→context-window question that `03-config.md` raised for `window_tokens`
auto-detect (its `[context].window_tokens = 0 → auto-detect` knob and its
"model→window table vs. explicit number" OPEN): the harness can read the window
straight off the result instead of maintaining a table.

**Caveat:** `contextWindow` is reported only at **result time** (terminal), so it
cannot seed the *live* occupancy % during the first turn. Live tracking therefore
still needs the configured `[context].window_tokens` (or a model→window default);
once a `result` arrives, the harness can confirm/correct the window for the run.
Cross-reference: `03-config.md` §`context.window_tokens` and its OPEN.

## Usage-limit / rate-limit detection (load-bearing for auto-resume)

On subscription usage exhaustion (`00-overview.md`, `01` §Guardrails) the harness
must (a) recognize it and (b) extract a **reset time** to sleep until. The
original draft called the reset-time parse "the riskiest parse in the system,"
expecting to scrape a human phrase out of an error. **The capture removed that
risk structurally:** there is a dedicated, out-of-band event with a clean epoch.

- **Signal — `rate_limit_event` (NEW).** Exact observed shape:

      {"type":"rate_limit_event",
       "rate_limit_info":{
         "status":"allowed",
         "resetsAt":1779691800,
         "rateLimitType":"five_hour",
         "overageStatus":"rejected",
         "overageDisabledReason":"org_level_disabled",
         "isUsingOverage":false},
       "uuid":"…","session_id":"…"}

- **Reset time — `rate_limit_info.resetsAt` is a UNIX EPOCH (seconds).** No
  fragile "resets at 3pm" / "try again in N minutes" parsing is needed; the
  harness sleeps until `resetsAt`. `rateLimitType` observed `"five_hour"`.
- **`status` observed value: `"allowed"`** (i.e. the window is *not* exhausted —
  this event also fires during normal operation as a state report). **OPEN:** the
  exact `status` value(s) when the window **is** exhausted — we could not trigger
  a real limit. The parser treats any `status` other than `""`/`"allowed"` as
  limited, which errs toward *pausing* rather than aborting an unattended run.
- **Fallback:** if no `resetsAt`/limit signal is available, wait `[usage].backoff`
  (`03-config.md`).
- **Distinguish** a usage-limit (→ *wait/auto-resume*, not a failure) from a
  genuine agent error (→ guardrail/halt path; see `api_error_status` /
  `is_error` / non-zero exit on the `result`). Misclassifying a limit as an error
  would abort an unattended multi-day run.

### Classification (PINNED — `src/lib/stream` task 2.3)

`LoopObservation.Classify(exitCode int) → {success | usage_limit | error}` is the
single decision the loop driver and the usage-wait guardrail branch on. Rules,
in order (usage-limit checked FIRST — it must win over the generic error path):

1. **usage_limit** if `LoopObservation.UsageLimited`. That flag is set
   comprehensively during the fold from EITHER signal:
   - the out-of-band `rate_limit_event` with a non-`"allowed"`/non-empty `status`
     (the clean, primary path), OR
   - a usage-limit **`result`** — `api_error_status` containing `429`, or the
     result text matching a known limit phrase (`usage limit reached`,
     `usage limit exceeded`, `rate limit exceeded`, `rate_limit_error`,
     `too many requests`, case-insensitive). HTTP **529 (overloaded) is NOT a
     usage limit** — it is transient server overload and stays on the error path.
2. **error** if `is_error`, a non-empty `api_error_status`, or a non-zero process
   exit. (A bare non-zero exit with no result — e.g. a timeout kill — is an error:
   the supervisor's exit code is the signal when a killed stream never reaches a
   `result`.)
3. **success** otherwise. NB: "success" means the *invocation* ran clean, **not**
   that the task is done — done-ness is the test gate's call (`01` §done-detection).

**Reset time.** Trust order: (1) the `rate_limit_event` epoch (`resetsAt`);
(2) an epoch parsed from the result text — the historical
`Claude AI usage limit reached|<epoch>` form, `<epoch>` being Unix seconds after
the final `|` (millisecond epochs tolerated). The event epoch overrides a
text-parsed one regardless of event order. When neither is present, `ResetAt` is
nil and the caller (guardrail 3.12) falls back to `[usage].backoff`.

**Bias.** The exact exhausted wording is still unverified (see OPEN), so the
phrase/`status` matching leans toward *treating ambiguous limit-like signals as a
limit* (pause) rather than aborting an unattended run; `[usage].max_cycles` caps
any runaway wait loop.

## Subagents — spawns, naming, and attribution

A **subagent spawn** is a `tool_use` block whose `name` is **`Agent`** (CLI
2.1.150's actual tool name) — or **`Task`** in other builds. The parser accepts
**both**. The tool `input` carries `{description, prompt, subagent_type}`.

- `subagent_type` (e.g. `"general-purpose"`) is the agent **class** only. The wire
  does **NOT** carry the subagent's model or effort. **This resolves the draft's
  OPEN about subagent model/effort discoverability: you get the class name only;**
  model/effort are resolved from `[subagents]` config (`07-agents-and-models.md`).
- **Attribution via `parent_tool_use_id`.** Inner events of the subagent carry
  `parent_tool_use_id == the spawning tool_use id`. This is how the harness (a)
  attaches nested activity to the right node in the AGENTS tree (`04-tui.md`), and
  (b) **excludes subagent tokens from the LEAD agent's context-occupancy count** —
  a subagent runs in its own context, so its usage must not inflate the lead's
  pressure %. Lead-agent events have an empty `parent_tool_use_id`.

## Parser placement & shape

- Lives in `src/lib/stream` (project standard library) as a reusable, streaming
  decoder: raw `io.Reader` → typed Go event values on a channel, plus a derived
  `LoopObservation` (tokens, cost, tool calls, subagent spawns, result/error,
  usage-limit + reset). One source of truth; the TUI, journal, and guardrails all
  consume the same typed stream (no ad-hoc re-parsing per consumer).
- The common envelope (`type`/`uuid`/`session_id`/`parent_tool_use_id`) is decoded
  first; type-specific payloads second. A payload-level decode failure is
  non-fatal — the envelope event is still returned with raw bytes intact, so the
  journal never loses a line.
- Unknown `type`s and unknown content blocks are preserved verbatim (`Raw`) for
  the journal but ignored by feature logic (forward-compatible with CLI changes).
- **Fixtures are the contract:** `src/lib/stream/testdata/basic.jsonl` and
  `subagent.jsonl` are verbatim 2.1.150 captures; the parser test asserts field
  names/paths against them. Re-capture and update the fixtures when bumping the
  pinned CLI version.

## OPEN

- Exact `status` wording in `rate_limit_event.rate_limit_info` when the window is
  **exhausted** (only `"allowed"` captured; no real limit triggered). Also the
  full set of error `result.subtype` values. The classifier above (task 2.3) is a
  best-effort heuristic chosen *because* of this gap — it matches known phrasings
  and an API 429 conservatively, biased toward pausing. **Re-verify against a real
  limited transcript when one can be captured** and tighten the phrase set if the
  observed wording differs.
- Whether cache tokens genuinely count toward the context window for the pressure
  estimate. **Decision regardless:** over-count (include cache) so trips fire early.
- **Outbound** (`--input-format stream-json`) message envelope for the soft
  wind-down / `discuss` injection — **not yet captured**; only the *output* side
  is pinned.
- Live-window seeding before the first `result`: `modelUsage.contextWindow`
  arrives only at result time, so the configured `[context].window_tokens` (or a
  model→window default) is still needed for the first turn (`03-config.md`).
