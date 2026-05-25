package loop

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"flanders/src/lib/config"
	"flanders/src/lib/journal"
	"flanders/src/lib/stream"
	"flanders/src/lib/supervise"
	"flanders/src/lib/task"
)

// composePrompt builds the prompt body for one task's loop. This is the MINIMAL
// composition (plan task 3.1): the current task file verbatim plus a one-line
// done/left plan summary — the spec's cost/quality lever is "inject only what the
// current task needs, never the whole plan or journal" (spec 01 §Prompt
// composition). The richer composition — dependency outcomes and named spec
// excerpts — is plan task 3.2 and slots in here; nothing about this version is a
// stub, it simply injects the two cheapest, highest-signal pieces.
//
// The task file is injected verbatim (frontmatter + body) because that single file
// already carries the description, acceptance criterion, and deps the loop needs,
// and re-emitting it keeps the harness from paraphrasing (and so drifting from)
// the on-disk truth the agent will edit.
func composePrompt(t *task.Task, store *task.Store) (string, error) {
	data, err := t.Bytes()
	if err != nil {
		return "", fmt.Errorf("serialize task file: %w", err)
	}

	var b strings.Builder
	b.WriteString("# Current task: ")
	b.WriteString(t.ID())
	b.WriteString("\n\nWork this one task to completion in a single focused pass, then\n")
	b.WriteString("update its `status` (`done`, or `blocked` with a `reason`).\n\n")
	b.WriteString("## Task file (specs/tasks/")
	b.WriteString(t.ID())
	b.WriteString(")\n\n```markdown\n")
	b.Write(data)
	if !bytes.HasSuffix(data, []byte("\n")) {
		b.WriteByte('\n')
	}
	b.WriteString("```\n\n")
	b.WriteString(planSummary(store))
	b.WriteByte('\n')
	return b.String(), nil
}

// planSummary is the "compact done/left summary of the overall plan" the prompt
// composition calls for (spec 01) — one line, so it orients the agent without
// dragging the whole plan into context. It counts the on-disk task statuses.
func planSummary(store *task.Store) string {
	var done, blocked, pending, active int
	for _, t := range store.Tasks() {
		switch t.Status() {
		case task.StatusDone:
			done++
		case task.StatusBlocked:
			blocked++
		case task.StatusActive:
			active++
		default:
			pending++
		}
	}
	total := len(store.Tasks())
	return fmt.Sprintf("## Plan progress\n\n%d/%d tasks done (%d pending, %d active, %d blocked). "+
		"Do not work any task but the one above.", done, total, pending, active, blocked)
}

// buildSummary assembles the journal record for one loop from the observation, the
// timing, and the task's status before/after the loop (spec 01 §journal). It is
// the single place the LoopObservation's wire-derived fields are mapped onto the
// journal's stable on-disk schema, so the journal package stays decoupled from the
// stream protocol (journal package doc).
func (d *Driver) buildSummary(
	phase string,
	t *task.Task,
	sessionID string,
	class config.AgentClass,
	res *supervise.Result,
	obs *stream.LoopObservation,
	start time.Time,
	outcome stream.Outcome,
) *journal.Summary {
	// Prefer the CLI's own reported duration (result.duration_ms) when present; fall
	// back to the harness wall-clock (e.g. a killed loop never reports a duration).
	durMS := res.Duration.Milliseconds()
	if obs.DurationMS > 0 {
		durMS = obs.DurationMS
	}

	sum := &journal.Summary{
		Phase:      phase,
		Task:       t.ID(),
		SessionID:  sessionID,
		StartedAt:  start,
		EndedAt:    d.now(),
		DurationMS: durMS,
		Model:      class.Model,
		Effort:     class.Effort,
		Cost:       obs.Cost,
		Tokens: journal.Tokens{
			Input:         obs.FinalUsage.InputTokens,
			Output:        obs.FinalUsage.OutputTokens,
			CacheRead:     obs.FinalUsage.CacheReadInputTokens,
			CacheCreation: obs.FinalUsage.CacheCreationInputTokens,
		},
		// StatusBefore is the status at selection — always `pending`, since Next only
		// returns pending tasks (spec 01 §select). Recorded for an honest transition.
		StatusBefore: t.Status(),
		// Test is left zero (Ran=false): the harness-owned test gate is the verify
		// step (plan task 3.4), which this driver does not yet run. Ran=false honestly
		// says "tests were not run this loop" rather than implying a pass/fail.
		Test: journal.TestResult{},
	}

	for _, sa := range obs.Subagents {
		sum.Subagents = append(sum.Subagents, journal.Subagent{Name: sa.SubagentType})
	}

	// StatusAfter / Reason: re-read the task file from disk, because the agent edits
	// its own `status` during the loop (spec 02 §Mutation ownership) and the in-memory
	// task predates those edits. A read failure (e.g. the agent deleted the file)
	// falls back to the pre-loop status rather than failing the journal write.
	if after, err := task.ParseFile(t.Path); err == nil {
		sum.StatusAfter = after.Status()
		sum.Reason = after.Reason()
	} else {
		sum.StatusAfter = t.Status()
	}

	sum.Error = errorText(res, obs, outcome)
	return sum
}

// errorText renders the journal's one-line error field for a non-clean loop, or ""
// for a clean one. A timeout is named explicitly (it has no result event to read);
// otherwise an OutcomeError uses the API status or result text, and a usage limit
// is recorded so the history shows why the run paused.
func errorText(res *supervise.Result, obs *stream.LoopObservation, outcome stream.Outcome) string {
	switch {
	case res.TimedOut:
		return "iteration timed out (guardrails.iteration_timeout)"
	case outcome == stream.OutcomeUsageLimit:
		return "usage limit reached"
	case outcome == stream.OutcomeError:
		if obs.APIErrorStatus != "" {
			return "api error: " + obs.APIErrorStatus
		}
		if obs.ResultText != "" {
			return obs.ResultText
		}
		return "agent or process error"
	default:
		return ""
	}
}
