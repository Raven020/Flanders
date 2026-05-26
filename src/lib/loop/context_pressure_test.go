package loop

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"flanders/src/lib/config"
	"flanders/src/lib/invoke"
	"flanders/src/lib/stream"
	"flanders/src/lib/supervise"
	"flanders/src/lib/task"
)

// fakeProc is a stand-in for *supervise.Proc (which cannot be constructed outside
// its package) so the context-guard tier logic can be exercised without a live
// process. It records the harness's tier actions: the tier-2 wind-down injects and
// the tier-3 kills.
type fakeProc struct {
	mu        sync.Mutex
	injects   []string
	kills     int
	injectErr error
}

func (f *fakeProc) Inject(text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.injects = append(f.injects, text)
	return f.injectErr
}

func (f *fakeProc) Kill() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kills++
}

func (f *fakeProc) injectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.injects)
}

func (f *fakeProc) killCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kills
}

// newGuard builds a context guard over the default config with the given
// stream_input setting (the only config knob the tier choice depends on). The
// default window is 200000 tokens at soft 0.75 / hard 0.90, so the usage figures in
// the tests below map directly onto the tiers.
func newGuard(streamInput bool) *contextGuard {
	cfg := config.Default()
	cfg.Agent.StreamInput = streamInput
	return newContextGuard(&cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), windDownMessage)
}

// usageEvent builds a lead-agent assistant event carrying `total` input tokens —
// the signal the tracker folds into occupancy. No parent_tool_use_id ⇒ it counts
// as lead context (subagent usage is excluded by leadUsage).
func usageEvent(total int) *stream.Event {
	return &stream.Event{
		Type:      stream.TypeAssistant,
		Assistant: &stream.APIMessage{Usage: &stream.Usage{InputTokens: total}},
	}
}

// TestContextGuardSoftInjectsOnce: crossing soft_pct with stream_input on injects
// the wind-down message exactly once (idempotent across further soft-band events)
// and never kills — tier 2, the graceful path.
func TestContextGuardSoftInjectsOnce(t *testing.T) {
	g := newGuard(true)
	p := &fakeProc{}
	g.handle(p, usageEvent(150_000)) // 0.75 → soft
	g.handle(p, usageEvent(160_000)) // 0.80 → still soft, must not re-inject

	if g.peakTrip() != stream.TripSoft {
		t.Errorf("peakTrip = %v, want soft", g.peakTrip())
	}
	if g.hardKilled() {
		t.Error("hardKilled = true, want false (only the soft tier tripped)")
	}
	if got := p.injectCount(); got != 1 {
		t.Fatalf("inject count = %d, want exactly 1 (inject once)", got)
	}
	if p.injects[0] != windDownMessage {
		t.Errorf("injected %q, want the wind-down message", p.injects[0])
	}
	if got := p.killCount(); got != 0 {
		t.Errorf("kills = %d, want 0 (the soft tier never kills)", got)
	}
}

// TestContextGuardHardKillsOnce: crossing hard_pct kills the process exactly once
// (idempotent across buffered post-kill events) and marks hardKilled — tier 3, the
// backstop. Jumping straight into the hard band injects no wind-down.
func TestContextGuardHardKillsOnce(t *testing.T) {
	g := newGuard(true)
	p := &fakeProc{}
	g.handle(p, usageEvent(185_000)) // 0.925 → hard
	g.handle(p, usageEvent(195_000)) // 0.975 → still hard, must not re-kill

	if g.peakTrip() != stream.TripHard {
		t.Errorf("peakTrip = %v, want hard", g.peakTrip())
	}
	if !g.hardKilled() {
		t.Error("hardKilled = false, want true (the hard backstop fired)")
	}
	if got := p.killCount(); got != 1 {
		t.Fatalf("kills = %d, want exactly 1 (kill once)", got)
	}
	if got := p.injectCount(); got != 0 {
		t.Errorf("injects = %d, want 0 (a jump straight to hard never winds down)", got)
	}
}

// TestContextGuardSoftThenHardEscalates: a loop that crosses soft and then hard
// both winds down (once) and then kills (once) — the escalation path when the agent
// kept consuming context after the wind-down nudge.
func TestContextGuardSoftThenHardEscalates(t *testing.T) {
	g := newGuard(true)
	p := &fakeProc{}
	g.handle(p, usageEvent(150_000)) // soft → inject
	g.handle(p, usageEvent(190_000)) // hard → kill

	if g.peakTrip() != stream.TripHard {
		t.Errorf("peakTrip = %v, want hard", g.peakTrip())
	}
	if !g.hardKilled() {
		t.Error("hardKilled = false, want true")
	}
	if got := p.injectCount(); got != 1 {
		t.Errorf("injects = %d, want 1 (the soft wind-down before escalation)", got)
	}
	if got := p.killCount(); got != 1 {
		t.Errorf("kills = %d, want 1", got)
	}
}

// TestContextGuardFallthroughNoStreamInput is the documented fallthrough (spec 03
// §stream_input, plan task 3.11 audit note 1): with stream_input off there is no
// stdin channel for the soft wind-down, so the soft band takes NO action and the
// guard goes straight from tier 1 (the rule) to tier 3 (hard kill) at hard_pct. The
// peak still records that soft was reached so the meter/journal are honest.
func TestContextGuardFallthroughNoStreamInput(t *testing.T) {
	g := newGuard(false)
	p := &fakeProc{}
	g.handle(p, usageEvent(160_000)) // soft band, but no channel → no inject
	if got := p.injectCount(); got != 0 {
		t.Errorf("injects = %d, want 0 (no stdin channel when stream_input is off)", got)
	}
	if g.peakTrip() != stream.TripSoft {
		t.Errorf("peakTrip = %v, want soft (the band was reached even with no action)", g.peakTrip())
	}
	if p.killCount() != 0 {
		t.Errorf("kills = %d, want 0 before the hard band", p.killCount())
	}

	g.handle(p, usageEvent(190_000)) // hard → the fallthrough kill
	if !g.hardKilled() || p.killCount() != 1 {
		t.Errorf("after hard band: hardKilled=%v kills=%d, want true/1", g.hardKilled(), p.killCount())
	}
}

// TestContextGuardStaysQuietBelowSoft: under soft_pct nothing happens — no inject,
// no kill, peak None. The guardrail must not disturb a healthy loop.
func TestContextGuardStaysQuietBelowSoft(t *testing.T) {
	g := newGuard(true)
	p := &fakeProc{}
	g.handle(p, usageEvent(100_000)) // 0.50 → none
	g.handle(p, usageEvent(140_000)) // 0.70 → still none

	if g.peakTrip() != stream.TripNone {
		t.Errorf("peakTrip = %v, want none", g.peakTrip())
	}
	if g.hardKilled() || p.injectCount() != 0 || p.killCount() != 0 {
		t.Errorf("below soft: hardKilled=%v injects=%d kills=%d, want false/0/0",
			g.hardKilled(), p.injectCount(), p.killCount())
	}
}

// TestContextOverreachHandoff locks the harness-written handoff note (the tier-3
// backstop's words the fresh split pass reads): it names the block reason, lists the
// touched files when there are any, says so plainly when there are none, and states
// the task must be split fresh.
func TestContextOverreachHandoff(t *testing.T) {
	withFiles := contextOverreachHandoff([]string{"a.go", "b.go"})
	for _, want := range []string{"context-overreach", "a.go", "b.go", "split"} {
		if !strings.Contains(withFiles, want) {
			t.Errorf("handoff missing %q:\n%s", want, withFiles)
		}
	}
	none := contextOverreachHandoff(nil)
	if !strings.Contains(none, "No working-tree changes") {
		t.Errorf("empty-files handoff should say no changes:\n%s", none)
	}
}

// TestIterateContextHardKillMarksBlocked is the headline 3.11 acceptance through the
// REAL supervisor (no live `claude`, per plan 2.5/3.1): a loop whose lead occupancy
// crosses hard_pct is hard-killed by the harness, which then OWNS recording the
// outcome — the task file ends up `blocked: context-overreach` with a handoff note,
// reconciliation respects that terminal status, and the journal names the backstop.
func TestIterateContextHardKillMarksBlocked(t *testing.T) {
	// The assistant turn reports 191k tokens against the 200k default window
	// (0.955 → hard). The result line lets the observation fold cleanly.
	const fixture = `{"type":"system","subtype":"init","session_id":"s1","model":"opus","permissionMode":"bypassPermissions","tools":[]}
{"type":"assistant","session_id":"s1","message":{"role":"assistant","model":"opus","content":[{"type":"text","text":"working hard"}],"usage":{"input_tokens":190000,"output_tokens":1000,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}
{"type":"result","subtype":"success","is_error":false,"session_id":"s1","total_cost_usd":0.5,"usage":{"input_tokens":190000,"output_tokens":1000}}
`
	cfg, p, jr := setupProject(t, mkTask("0001", task.StatusPending, nil))
	taskPath := filepath.Join(p.Tasks, "0001.md")
	fixturePath := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(fixturePath, []byte(fixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	d, err := New(Options{Config: cfg, Paths: p, Journal: jr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Run the fixture through the REAL supervisor so the OnEvent hook drives the
	// live tracker and the hard kill fires against a real process.
	d.run = func(ctx context.Context, spec supervise.Spec) (*supervise.Result, error) {
		spec.Command = invoke.Command{Bin: "cat", Args: []string{fixturePath}}
		spec.StreamInput = false
		return supervise.Run(ctx, spec)
	}

	res, err := d.Iterate(context.Background(), "build", 1)
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}

	if res.ContextTrip != stream.TripHard {
		t.Errorf("ContextTrip = %v, want hard", res.ContextTrip)
	}

	// The harness wrote the block marker + handoff to the task file itself.
	after, err := task.ParseFile(taskPath)
	if err != nil {
		t.Fatalf("ParseFile after loop: %v", err)
	}
	if after.Status() != task.StatusBlocked || after.Reason() != task.ReasonContextOverreach {
		t.Errorf("on-disk status/reason = %q/%q, want blocked/context-overreach", after.Status(), after.Reason())
	}
	if !strings.Contains(after.Notes(), "context-overreach") {
		t.Errorf("task notes missing the handoff:\n%s", after.Notes())
	}

	// Reconciliation respected the harness-written terminal status.
	if res.Reconcile.To != task.StatusBlocked {
		t.Errorf("Reconcile.To = %q, want blocked", res.Reconcile.To)
	}

	// The journal records the transition and names the backstop, so the history is
	// honest about why the loop ended.
	sum, err := jr.Read(res.JournalSeq)
	if err != nil {
		t.Fatalf("journal.Read: %v", err)
	}
	if sum.StatusAfter != task.StatusBlocked || sum.Reason != task.ReasonContextOverreach {
		t.Errorf("journal = %q/%q, want blocked/context-overreach", sum.StatusAfter, sum.Reason)
	}
	if !strings.Contains(sum.Error, "context-pressure hard backstop") {
		t.Errorf("journal Error = %q, want the backstop name", sum.Error)
	}
}
