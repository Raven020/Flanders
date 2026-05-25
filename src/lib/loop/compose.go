package loop

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"flanders/src/lib/config"
	"flanders/src/lib/journal"
	"flanders/src/lib/stream"
	"flanders/src/lib/supervise"
	"flanders/src/lib/task"
	"flanders/src/lib/verify"
)

// composePrompt builds the prompt body for one task's loop — the cost/quality lever
// (spec 01 §Prompt composition). With a fresh context every iteration, the dominant
// lever for both cost and quality is injecting ONLY what the current task needs,
// never the whole plan or journal. So the body is exactly the five pieces the spec
// names, in order:
//
//  1. the current task file verbatim (description + acceptance + deps),
//  2. the outcomes of the tasks it depends on (so the agent builds on them, not
//     re-derives them),
//  3. only the spec excerpts the task explicitly references (`specs/x.md §Section`),
//  4. a one-line done/remaining summary of the overall plan,
//  5. (the loop rules go via --append-system-prompt, handled by the caller).
//
// The task file is injected verbatim (frontmatter + body) because that single file
// already carries the description, acceptance criterion, and deps the loop needs,
// and re-emitting it keeps the harness from paraphrasing (and so drifting from) the
// on-disk truth the agent will edit. Everything else is a deliberately compact
// summary, never a second full file — that is the whole point of the lever.
//
// It is a method so it can resolve referenced spec files against the project root
// (d.paths.Root) and log a skipped reference (d.log); the per-section builders below
// stay pure free functions so they are unit-testable in isolation.
func (d *Driver) composePrompt(t *task.Task, store *task.Store) (string, error) {
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
	b.WriteString(dependencyOutcomes(t, store))
	b.WriteString(specExcerpts(t, d.paths.Root, d.log))
	b.WriteString(planSummary(store))
	b.WriteByte('\n')
	return b.String(), nil
}

// dependencyOutcomes summarizes the tasks the current one depends on — item 2 of the
// composition (spec 01). The selector only runs a task once all its deps are `done`,
// so these are completed work the current loop should build on rather than redo. The
// summary is deliberately frontmatter-derived only — id, status, the `acceptance`
// criterion it met, and any `files`/`notes` the dep recorded — NOT the dependency's
// full body: injecting the whole dep file would defeat the cost lever and is exactly
// the "never the whole plan" the spec warns against. Returns "" when there are no
// deps (so composePrompt emits nothing).
func dependencyOutcomes(t *task.Task, store *task.Store) string {
	deps := t.Deps()
	if len(deps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Dependency outcomes\n\n")
	b.WriteString("These tasks are complete; build on their results, do not redo them:\n\n")
	for _, dep := range deps {
		d := store.ByID(dep)
		if d == nil {
			// The selector would not have picked t with an unresolved dep, but be
			// defensive: record it so the agent isn't silently misled about its inputs.
			fmt.Fprintf(&b, "- **%s** — not found in the plan\n", strings.TrimSpace(dep))
			continue
		}
		fmt.Fprintf(&b, "- **%s** (%s)", d.ID(), d.Status())
		if acc := strings.TrimSpace(d.Acceptance()); acc != "" {
			fmt.Fprintf(&b, " — %s", acc)
		}
		b.WriteByte('\n')
		if files := d.Files(); len(files) > 0 {
			fmt.Fprintf(&b, "  - files: %s\n", strings.Join(files, ", "))
		}
		if notes := strings.TrimSpace(d.Notes()); notes != "" {
			fmt.Fprintf(&b, "  - notes: %s\n", oneLine(notes))
		}
	}
	b.WriteByte('\n')
	return b.String()
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

// specRefRe matches a task's reference to a spec section, e.g.
//
//	References: specs/01-ralph-loop.md §Iteration anatomy.
//
// Group 1 is the file path (any path ending in .md, so a configured non-`specs/`
// layout still works — the author writes the real path); group 2 is the section name
// after the § up to end of line. The § is the strong, intentional signal of a spec
// reference, so a bare `foo.md` mention without one is ignored.
var specRefRe = regexp.MustCompile(`([^\s§]+\.md)\s*§\s*([^\n]+)`)

// specRef is one parsed `<path> §<section>` reference from a task body.
type specRef struct {
	path    string
	section string
}

// specExcerpts injects ONLY the spec sections the task explicitly references — item 3
// of the composition (spec 01: "only the named spec excerpts the task references").
// References are parsed from the task body; each is resolved to a file under root and
// the named heading's section is extracted (heading through the next same-or-higher
// heading). Unresolvable references (file or section missing, or a path that escapes
// the project) are logged at debug and skipped rather than failing the loop: a stale
// reference is a spec-authoring slip, not a harness fault, and the agent can still
// delegate a subagent to read the file. Returns "" when nothing resolves, so the
// section header only appears when there is real content under it.
func specExcerpts(t *task.Task, root string, log *slog.Logger) string {
	refs := parseSpecRefs(t.Body)
	if len(refs) == 0 {
		return ""
	}
	var b strings.Builder
	seen := make(map[string]bool, len(refs))
	for _, r := range refs {
		key := r.path + "\x00" + normalizeHeading(r.section)
		if seen[key] {
			continue
		}
		seen[key] = true

		content, ok := readSpecSection(root, r.path, r.section)
		if !ok {
			if log != nil {
				log.Debug("spec excerpt not resolved", "path", r.path, "section", r.section)
			}
			continue
		}
		if b.Len() == 0 {
			b.WriteString("## Referenced spec excerpts\n\n")
			b.WriteString("Only the spec sections this task names are included:\n\n")
		}
		fmt.Fprintf(&b, "From %s §%s:\n\n%s\n\n", r.path, r.section, content)
	}
	return b.String()
}

// parseSpecRefs pulls the `<path> §<section>` references out of a task body, cleaning
// trailing sentence punctuation off the section name (so "§Iteration anatomy." names
// the heading "Iteration anatomy") and stripping any leading quoting/bracket char a
// path picked up (e.g. an inline-code backtick).
func parseSpecRefs(body string) []specRef {
	matches := specRefRe.FindAllStringSubmatch(body, -1)
	refs := make([]specRef, 0, len(matches))
	for _, m := range matches {
		path := strings.TrimLeft(m[1], "`'\"([{<")
		section := strings.TrimSpace(m[2])
		section = strings.TrimRight(section, " \t.;,")
		section = strings.TrimSpace(section)
		if path == "" || section == "" {
			continue
		}
		refs = append(refs, specRef{path: path, section: section})
	}
	return refs
}

// readSpecSection reads the referenced spec file (resolved under root) and returns
// the named section. It refuses an absolute path or one that escapes root so a task
// reference can never make the harness read outside the project tree. A missing file
// or section yields ("", false) — the caller skips it.
func readSpecSection(root, refPath, section string) (string, bool) {
	clean := filepath.Clean(refPath)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(root, clean))
	if err != nil {
		return "", false
	}
	return extractSection(string(data), section)
}

// extractSection returns the markdown section named by `name` from `md`: the matching
// heading line through (but not including) the next heading of the same or higher
// level. Headings inside fenced code blocks are ignored so a `#` comment in a shell
// example can't be mistaken for a section. Matching is fuzzy-but-safe (see
// headingMatches) to tolerate the trailing punctuation and parenthetical suffixes
// real spec headings carry. Returns ("", false) when no heading matches.
func extractSection(md, name string) (string, bool) {
	lines := strings.Split(md, "\n")
	target := normalizeHeading(name)

	start, startLevel := -1, 0
	inFence := false
	for i, line := range lines {
		if isFence(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		lvl, text, ok := heading(line)
		if !ok {
			continue
		}
		if start < 0 {
			if headingMatches(text, target) {
				start, startLevel = i, lvl
			}
			continue
		}
		if lvl <= startLevel {
			return strings.TrimRight(strings.Join(lines[start:i], "\n"), "\n"), true
		}
	}
	if start < 0 {
		return "", false
	}
	return strings.TrimRight(strings.Join(lines[start:], "\n"), "\n"), true
}

// heading parses an ATX markdown heading (`## Title`), returning its level (1–6) and
// trimmed text. A line that is not a heading returns ok=false.
func heading(line string) (level int, text string, ok bool) {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(line) || line[n] != ' ' {
		return 0, "", false
	}
	return n, strings.TrimSpace(line[n+1:]), true
}

// isFence reports whether a line opens or closes a fenced code block (``` or ~~~).
func isFence(line string) bool {
	s := strings.TrimSpace(line)
	return strings.HasPrefix(s, "```") || strings.HasPrefix(s, "~~~")
}

// headingMatches reports whether a heading's text satisfies a (normalized) reference
// target. An exact match wins; otherwise either side being a prefix of the other is
// accepted, which absorbs the two realities of hand-written references: a heading
// carries a parenthetical the reference omits ("Prompt composition" vs "Prompt
// composition (the cost/quality lever)"), or the reference trails extra words past
// the heading. Both inputs are already normalized.
func headingMatches(headingText, target string) bool {
	h := normalizeHeading(headingText)
	if h == "" || target == "" {
		return false
	}
	return h == target || strings.HasPrefix(h, target) || strings.HasPrefix(target, h)
}

// normalizeHeading lowercases, collapses internal whitespace, and strips trailing
// sentence punctuation so "Iteration anatomy." and "iteration  anatomy" compare equal.
func normalizeHeading(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimRight(s, " \t.;,:")
	return strings.Join(strings.Fields(s), " ")
}

// oneLine collapses all whitespace (including newlines) to single spaces so a
// multi-line notes field stays a single compact bullet in the dependency summary.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// buildSummary assembles the journal record for one loop from the observation, the
// timing, and the task's status before/after the loop (spec 01 §journal). It is the
// single place the LoopObservation's wire-derived fields are mapped onto the
// journal's stable on-disk schema, so the journal package stays decoupled from the
// stream protocol (journal package doc).
func (d *Driver) buildSummary(
	phase string,
	t *task.Task, // the task as selected (pre-loop): source of StatusBefore
	after *task.Task, // the task after the loop + reconciliation: source of StatusAfter/Reason
	sessionID string,
	class config.AgentClass,
	res *supervise.Result,
	obs *stream.LoopObservation,
	start time.Time,
	outcome stream.Outcome,
	vr *verify.Result,
	files []string, // paths touched this loop, from git diff (empty if none / non-repo)
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
		// Files touched this loop, inferred by the harness from git diff (spec 02);
		// empty when nothing changed or the target is not a git repo.
		Files: files,
	}

	// Test records the ground-truth gate verdict (spec 01 §journal: "test result").
	// vr is nil when the verify step did not run this loop (a non-code phase, or a
	// non-clean invocation — see runsTestGate); the zero TestResult then honestly
	// reports Ran=false ("not verified this loop") rather than implying a pass/fail.
	if vr != nil {
		sum.Test = journal.TestResult{
			Command:  vr.Test.Command,
			Ran:      vr.Test.Ran,
			ExitCode: vr.Test.ExitCode,
		}
	}

	for _, sa := range obs.Subagents {
		sum.Subagents = append(sum.Subagents, journal.Subagent{Name: sa.SubagentType})
	}

	// StatusAfter / Reason come from `after`: the task as it stands once the agent's
	// own mid-run edits and the harness's reconciliation (spec 02 §Mutation ownership)
	// have both been applied. So the journal records the net, final transition for the
	// loop — pending → (agent flip | harness promotion | normalization).
	sum.StatusAfter = after.Status()
	sum.Reason = after.Reason()

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
