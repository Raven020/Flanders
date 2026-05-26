package loop

import (
	"fmt"
	"path/filepath"
	"strings"

	"flanders/src/lib/task"
)

// plancheck.go implements the plan-phase loop-exit condition (plan task 4.3): the
// mechanical plan-completeness coverage scan.
//
// Spec 06 §Plan-completeness criterion (locked) and spec 02 §Plan lifecycle define
// "complete enough to start building" as: every requirement in specs/*.md maps to at
// least one task in specs/tasks/*.md. The *method* for judging that was spec-OPEN
// (agent self-assessment vs. a coverage check vs. user approval). We pick the coverage
// check, for the same reason the harness owns the test gate rather than trusting the
// agent's self-report (spec 00 decision 2, spec 01 §done-detection): a mechanical scan
// is cheap, deterministic, and harness-owned ground truth. The plan agent is already
// told (planInstructions) to reference the spec section each task implements as
// `specs/NN-name.md §Section`; this scan checks exactly those references, so the gate
// aligns with the contract the agent works to.
//
// What counts as a "requirement". There is no machine-readable requirement marker in
// the specs, so we use the spec's own section structure: each level-2 (##) heading in a
// non-task spec file is one requirement unit. This is the granularity the specs are
// organized by and that tasks reference via §Section. We deliberately do NOT treat ###
// sub-headings as separate requirements — that would demand a task per sub-point and
// make the plan phase loop forever chasing completeness, exactly what spec 06 warns
// against ("we do not try to prove the plan is perfect up front"). Instead a deeper
// heading is an *alias* of its enclosing ## requirement: a task that references a finer
// subsection still credits the parent requirement.
//
// House-style meta sections are excluded (defaultExcludedSections). Spec 05
// §Spec-authoring conventions establishes the house style: explicit `OPEN` markers for
// *undecided* points, and 00-overview.md as an *index*. An `## OPEN` section is by
// definition not a settled requirement, and the overview's navigational sections
// (spec/keyword index, remaining open decisions) are not buildable — demanding a task
// for them would block the exit condition forever. Excluding them is what keeps the
// scan satisfiable.
//
// Cost asymmetry (why we lean toward not over-excluding). A false "uncovered" only
// costs an extra plan loop or two (bounded by the max-iterations guardrail, 3.8); a
// false "covered" lets a real gap reach the build phase, where spec 06's drain +
// batch-replan flow catches it as a blocked task — an explicitly accepted fallback. So
// the scan is a *floor* that catches gross omissions (a whole spec or section nobody
// referenced), not a proof of perfection.

// Requirement is one spec section the plan must cover: a level-2 (##) heading in a
// non-task spec file. It is the unit reported as a gap in Coverage.Uncovered so the
// orchestrator/TUI can name exactly what is missing (and feed it into the next focused
// plan loop's prompt).
type Requirement struct {
	Spec    string // spec file base name, e.g. "06-orchestration.md"
	Section string // the ## heading text, verbatim
}

// String renders a requirement the way a task body references it, e.g.
// "06-orchestration.md §Autonomy" — handy for logs and the gap list.
func (r Requirement) String() string { return r.Spec + " §" + r.Section }

// Coverage is the result of a plan-completeness scan (plan task 4.3). It is the
// plan-phase analogue of verify.Result: a harness-owned verdict the orchestrator reads
// to decide the plan-loop exit (Complete) plus the detail to act on a gap (Uncovered).
type Coverage struct {
	Specs        int           // non-task spec files scanned
	Tasks        int           // task files considered
	Requirements int           // total requirement units found (## headings, meta excluded)
	Covered      int           // requirements with >=1 task referencing them
	Uncovered    []Requirement // the gap list, in spec-file then document order
}

// Complete reports whether the plan covers every requirement — the plan-loop exit
// condition (spec 06 §Plan-completeness criterion). It is simply "no gaps remain".
//
// The vacuous case (Requirements == 0) is reported complete: if the specs contain no
// requirement sections there is nothing to cover, so looping further would never
// converge. That degenerate spec set is guarded separately — PlanComplete errors when
// there are no spec files at all, and an orchestrator can treat "0 requirements" or "0
// tasks" as suspicious — but the literal criterion ("every requirement maps to a task")
// is vacuously satisfied, and reporting otherwise would loop forever.
func (c *Coverage) Complete() bool { return len(c.Uncovered) == 0 }

// defaultExcludedSections are the spec house-style headings that are NOT build
// requirements (spec 05 §Spec-authoring conventions: `OPEN` markers for undecided
// points; 00-overview.md is an index). Entries are normalized (see normalizeHeading)
// and matched either exactly or as a "<entry> " prefix, so "OPEN", "OPEN questions",
// "Keyword index — where to look", and "Remaining open decisions (tracked)" are all
// excluded while a real heading like "Open ports" (none exist here) would need to be an
// exact/prefix match to be dropped.
var defaultExcludedSections = map[string]bool{
	"open":                     true, // every spec's `## OPEN` deferred-questions section
	"remaining open decisions": true, // 00-overview tracked-open list (same category as OPEN)
	"spec index":               true, // 00-overview navigational index
	"keyword index":            true, // 00-overview "Keyword index — where to look"
	"keywords":                 true, // a bare keyword listing, if ever a heading
}

// specReq is one requirement during the scan: the ## heading plus the normalized texts
// (the heading itself and every nested sub-heading) that a task reference may name to
// cover it. aliases is what makes the nested-credit rule work (see file doc).
type specReq struct {
	spec    string
	section string   // verbatim ## heading
	aliases []string // normalized: the ## heading + all nested sub-headings under it
}

// specRequirements extracts the requirement units from one spec file: its level-2 (##)
// headings, fence-aware (reusing isFence/heading from compose.go so a `#` comment inside
// a ```toml example — as in 03-config.md — is never mistaken for a heading), with the
// house-style meta sections excluded. Each requirement collects its nested sub-headings
// as aliases so a task referencing a finer ### subsection still credits the parent ##.
func specRequirements(specName, content string, excluded map[string]bool) []specReq {
	var reqs []specReq
	cur := -1 // index into reqs of the ## requirement currently open, or -1
	inFence := false
	for _, line := range strings.Split(content, "\n") {
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
		switch {
		case lvl <= 1:
			// The file title (one `#` per file) — not a requirement; it closes any
			// open ## so a stray later heading can't attach to the wrong section.
			cur = -1
		case lvl == 2:
			if isExcludedHeading(text, excluded) {
				cur = -1 // an excluded ## (e.g. OPEN); its sub-headings are excluded too
				continue
			}
			reqs = append(reqs, specReq{
				spec:    specName,
				section: text,
				aliases: []string{normalizeHeading(text)},
			})
			cur = len(reqs) - 1
		default: // lvl >= 3: a sub-heading rolls up to the open ## requirement
			if cur >= 0 {
				reqs[cur].aliases = append(reqs[cur].aliases, normalizeHeading(text))
			}
		}
	}
	return reqs
}

// isExcludedHeading reports whether a heading is a house-style meta section to skip. It
// normalizes the heading then matches each excluded entry exactly or as a leading
// "<entry> " word-prefix, so "Keyword index — where to look" matches "keyword index"
// without a bare "open" entry swallowing an unrelated "Open <something>" heading that
// merely contains the word.
func isExcludedHeading(text string, excluded map[string]bool) bool {
	norm := normalizeHeading(text)
	if excluded[norm] {
		return true
	}
	for e := range excluded {
		if strings.HasPrefix(norm, e+" ") {
			return true
		}
	}
	return false
}

// scanCoverage computes plan-completeness from already-read spec contents and the spec
// references parsed out of the task bodies. It is pure (no disk, no Driver) so the
// coverage rule is unit-testable in isolation; PlanComplete wires the disk reads into
// it. A requirement is covered when some task references its spec file (matched by base
// name, case-insensitively — the author writes `specs/NN.md` regardless of the
// configured specs dir) AND names its heading or any of its sub-headings (matched with
// the same fuzzy headingMatches rule the prompt composer uses, so a reference that
// trails punctuation or omits a parenthetical still resolves).
func scanCoverage(specs []planSpec, taskRefs []specRef, excluded map[string]bool) *Coverage {
	// Index the task references by spec base name → the normalized sections referenced.
	refsByBase := make(map[string][]string)
	for _, r := range taskRefs {
		base := strings.ToLower(filepath.Base(r.path))
		refsByBase[base] = append(refsByBase[base], normalizeHeading(r.section))
	}

	cov := &Coverage{Specs: len(specs)}
	for _, s := range specs {
		refSections := refsByBase[strings.ToLower(s.name)]
		for _, req := range specRequirements(s.name, s.content, excluded) {
			cov.Requirements++
			if requirementCovered(req, refSections) {
				cov.Covered++
			} else {
				cov.Uncovered = append(cov.Uncovered, Requirement{Spec: req.spec, Section: req.section})
			}
		}
	}
	return cov
}

// requirementCovered reports whether any referenced section (already normalized) matches
// the requirement's heading or one of its nested-subsection aliases.
func requirementCovered(req specReq, refSections []string) bool {
	for _, rs := range refSections {
		for _, alias := range req.aliases {
			if headingMatches(alias, rs) {
				return true
			}
		}
	}
	return false
}

// PlanComplete runs the mechanical plan-completeness coverage scan — the plan phase's
// loop-exit condition (plan task 4.3, spec 06 §Plan-completeness criterion; spec 02
// §Plan lifecycle). The orchestrator (Phase 5) calls PlanIterate repeatedly until this
// reports Complete; the uncovered list lets a focused re-plan target exactly the gaps.
//
// It reads the same ground truth off disk as the plan loop: the non-task specs
// (readPlanSpecs — so the tasks dir is never scanned as a spec) and the task files
// (task.LoadDir — which validates each, so a malformed task file the agent just wrote
// surfaces here as an error rather than a silently-miscounted plan). No spec files is an
// error (nothing to check), matching composePlanPrompt; an empty tasks dir is not (every
// requirement is simply uncovered — the expected first-plan-loop state).
func (d *Driver) PlanComplete() (*Coverage, error) {
	specs, err := d.readPlanSpecs()
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("no spec files (*.md) found under %s — nothing to check", d.paths.Specs)
	}
	store, err := task.LoadDir(d.paths.Tasks)
	if err != nil {
		return nil, fmt.Errorf("load tasks: %w", err)
	}
	var refs []specRef
	for _, t := range store.Tasks() {
		refs = append(refs, parseSpecRefs(t.Body)...)
	}
	cov := scanCoverage(specs, refs, defaultExcludedSections)
	cov.Tasks = len(store.Tasks())

	d.log.Info("plan-completeness scan",
		"specs", cov.Specs, "tasks", cov.Tasks,
		"requirements", cov.Requirements, "covered", cov.Covered,
		"uncovered", len(cov.Uncovered), "complete", cov.Complete())
	return cov, nil
}
