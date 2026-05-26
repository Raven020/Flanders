package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flanders/src/lib/stream"
	"flanders/src/lib/supervise"
	"flanders/src/lib/task"
)

// TestSpecRequirementsExtraction locks the requirement unit: level-2 (##) headings, with
// ### sub-headings folded in as aliases, headings inside a ```fence ignored, the file
// title ignored, and the house-style ## OPEN section excluded.
func TestSpecRequirementsExtraction(t *testing.T) {
	const md = "# Flanders — Sample\n" + // level-1 title: not a requirement
		"\n## Alpha\n\ntext\n" +
		"\n### Alpha detail\n\nmore\n" + // nested under Alpha → alias, not its own req
		"\n## Beta (locked)\n\ntext\n" +
		"\n```toml\n# not a heading\n## also not a heading\n```\n" + // fenced: skipped
		"\n## OPEN\n\n- a deferred question\n" // house-style meta: excluded

	reqs := specRequirements("07-x.md", md, defaultExcludedSections)
	if len(reqs) != 2 {
		t.Fatalf("got %d requirements, want 2 (Alpha, Beta); reqs=%+v", len(reqs), reqs)
	}
	if reqs[0].section != "Alpha" || reqs[1].section != "Beta (locked)" {
		t.Errorf("sections = %q, %q; want Alpha, Beta (locked)", reqs[0].section, reqs[1].section)
	}
	if !contains(reqs[0].aliases, "alpha detail") {
		t.Errorf("Alpha aliases = %v, want to include the nested 'alpha detail'", reqs[0].aliases)
	}
	if reqs[0].spec != "07-x.md" {
		t.Errorf("spec = %q, want 07-x.md", reqs[0].spec)
	}
}

// TestIsExcludedHeading: the meta exclusions are load-bearing (without them the scan
// never reaches "complete" because OPEN/index sections can't have tasks). Exact and
// "<entry> " prefix matches are excluded; a real requirement that merely starts with the
// same letters is NOT.
func TestIsExcludedHeading(t *testing.T) {
	excluded := []string{
		"OPEN", "open",
		"Keyword index — where to look",
		"Remaining open decisions (tracked)",
		"Spec index (all drafted)",
	}
	for _, h := range excluded {
		if !isExcludedHeading(h, defaultExcludedSections) {
			t.Errorf("isExcludedHeading(%q) = false, want true (house-style meta section)", h)
		}
	}
	kept := []string{"Autonomy", "Done-detection (locked: harness-owned)", "Openness of the API"}
	for _, h := range kept {
		if isExcludedHeading(h, defaultExcludedSections) {
			t.Errorf("isExcludedHeading(%q) = true, want false (a real requirement)", h)
		}
	}
}

// TestScanCoverage is the core coverage rule: a requirement is covered when a task
// references its spec (by base name) and its heading — with fuzzy heading matching (a
// reference may omit a parenthetical) and nested-subsection credit (a reference to a ###
// covers the parent ##). An unreferenced requirement lands in Uncovered.
func TestScanCoverage(t *testing.T) {
	specs := []planSpec{
		{name: "06-orchestration.md", content: "# T\n\n## Autonomy (locked)\n\nx\n\n## Phases\n\ny\n"},
		{name: "07-agents-and-models.md", content: "# T\n\n## Configurability (locked)\n\n### Defaults\n\nz\n"},
	}
	refs := []specRef{
		{path: "specs/06-orchestration.md", section: "Autonomy"},     // fuzzy: omits "(locked)"
		{path: "specs/07-agents-and-models.md", section: "Defaults"}, // nested → credits Configurability
		// nothing references 06 §Phases → it must be the lone gap.
	}
	cov := scanCoverage(specs, refs, defaultExcludedSections)
	if cov.Requirements != 3 || cov.Covered != 2 {
		t.Fatalf("Requirements=%d Covered=%d, want 3/2; cov=%+v", cov.Requirements, cov.Covered, cov)
	}
	if len(cov.Uncovered) != 1 || cov.Uncovered[0].Spec != "06-orchestration.md" || cov.Uncovered[0].Section != "Phases" {
		t.Fatalf("Uncovered = %+v, want [06-orchestration.md §Phases]", cov.Uncovered)
	}
	if cov.Complete() {
		t.Error("Complete() = true, want false (Phases uncovered)")
	}
	if got, want := cov.Uncovered[0].String(), "06-orchestration.md §Phases"; got != want {
		t.Errorf("Requirement.String() = %q, want %q", got, want)
	}
}

// TestCoverageComplete: the vacuous case (no requirements at all — a spec with only a
// title + OPEN) is reported complete (nothing to cover); a fully-referenced spec is too.
func TestCoverageComplete(t *testing.T) {
	empty := scanCoverage([]planSpec{{name: "x.md", content: "# Title only\n\n## OPEN\n\nq\n"}}, nil, defaultExcludedSections)
	if empty.Requirements != 0 || !empty.Complete() {
		t.Errorf("empty: requirements=%d complete=%v, want 0/true", empty.Requirements, empty.Complete())
	}
	specs := []planSpec{{name: "01.md", content: "# T\n\n## A\n\n## B\n"}}
	refs := []specRef{{path: "01.md", section: "A"}, {path: "01.md", section: "B"}}
	full := scanCoverage(specs, refs, defaultExcludedSections)
	if !full.Complete() || full.Covered != 2 || len(full.Uncovered) != 0 {
		t.Errorf("full: complete=%v covered=%d uncovered=%v, want true/2/none", full.Complete(), full.Covered, full.Uncovered)
	}
}

// TestPlanCompleteEndToEnd is the 4.3 acceptance: the Driver reads the real specs and
// task files off disk and reports which spec requirements no task covers. Here one task
// references 01 §Requirement (covered) while 02 §Another has no task (the gap); 01's
// ## OPEN is excluded, so it is never demanded.
func TestPlanCompleteEndToEnd(t *testing.T) {
	cfg, p, jr := setupProject(t,
		task.New("0001", task.StatusPending, nil, "feature test passes",
			"## Do it\n\nImplements specs/01-feature.md §Requirement.\n"),
	)
	writeSpec(t, p, "01-feature.md", "# Feature\n\n## Requirement\n\nDo the thing.\n\n## OPEN\n\n- a question\n")
	writeSpec(t, p, "02-more.md", "# More\n\n## Another\n\nMore stuff.\n")
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cov, err := d.PlanComplete()
	if err != nil {
		t.Fatalf("PlanComplete: %v", err)
	}
	if cov.Specs != 2 || cov.Tasks != 1 {
		t.Errorf("Specs=%d Tasks=%d, want 2/1", cov.Specs, cov.Tasks)
	}
	if cov.Requirements != 2 || cov.Covered != 1 { // Requirement covered; Another not; OPEN excluded
		t.Errorf("Requirements=%d Covered=%d, want 2/1; uncovered=%+v", cov.Requirements, cov.Covered, cov.Uncovered)
	}
	if len(cov.Uncovered) != 1 || cov.Uncovered[0].Spec != "02-more.md" || cov.Uncovered[0].Section != "Another" {
		t.Fatalf("Uncovered = %+v, want [02-more.md §Another]", cov.Uncovered)
	}
	if cov.Complete() {
		t.Error("Complete() = true, want false (02 §Another uncovered)")
	}
}

// TestPlanCompleteNoSpecsErrors: with no spec files there is nothing to check — the same
// infrastructure error composePlanPrompt raises, so the orchestrator surfaces it rather
// than treating an empty spec set as a complete plan.
func TestPlanCompleteNoSpecsErrors(t *testing.T) {
	cfg, p, jr := setupProject(t)
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := d.PlanComplete(); err == nil || !strings.Contains(err.Error(), "nothing to check") {
		t.Fatalf("PlanComplete with no specs: err=%v, want a 'nothing to check' error", err)
	}
}

// TestPlanIteratePopulatesPlanComplete wires 4.3 into the plan loop's result (the
// loop-exit signal the orchestrator reads): after a plan loop whose agent writes a task
// covering the one requirement, Result.PlanComplete reports the plan complete.
func TestPlanIteratePopulatesPlanComplete(t *testing.T) {
	cfg, p, jr := setupProject(t)
	writeSpec(t, p, "01-feature.md", "# Feature\n\n## Requirement\n\nDo the thing.\n")
	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.newSessionID = func() (string, error) { return "s", nil }
	d.run = func(_ context.Context, _ supervise.Spec) (*supervise.Result, error) {
		// The plan agent decomposes the requirement into a task that references it.
		if werr := os.WriteFile(filepath.Join(p.Tasks, "0001.md"), []byte(planTaskFile), 0o644); werr != nil {
			t.Fatalf("plan agent write task: %v", werr)
		}
		return &supervise.Result{Observation: &stream.LoopObservation{Done: true, Subtype: "success"}, ExitCode: 0}, nil
	}

	res, err := d.PlanIterate(context.Background(), 1)
	if err != nil {
		t.Fatalf("PlanIterate: %v", err)
	}
	if res.PlanComplete == nil {
		t.Fatal("Result.PlanComplete = nil, want a coverage verdict (the plan-loop exit signal)")
	}
	if !res.PlanComplete.Complete() {
		t.Errorf("PlanComplete.Complete() = false, want true; uncovered=%+v", res.PlanComplete.Uncovered)
	}
	if res.PlanComplete.Tasks != 1 || res.PlanComplete.Requirements != 1 {
		t.Errorf("coverage = %d tasks / %d reqs, want 1/1", res.PlanComplete.Tasks, res.PlanComplete.Requirements)
	}
}
