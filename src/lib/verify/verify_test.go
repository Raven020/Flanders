package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flanders/src/lib/config"
)

// TestRunTestPasses: the canonical gate is the test command exiting 0.
func TestRunTestPasses(t *testing.T) {
	r := Run(context.Background(), t.TempDir(), config.Commands{Test: "exit 0"})
	if !r.Passed() {
		t.Errorf("Passed() = false, want true for `exit 0`")
	}
	if !r.Test.Ran || r.Test.ExitCode != 0 || r.Test.Err != nil {
		t.Errorf("test result = %+v, want Ran=true ExitCode=0 Err=nil", r.Test)
	}
	if r.Test.Command != "exit 0" {
		t.Errorf("Command = %q, want %q", r.Test.Command, "exit 0")
	}
}

// TestRunTestFails: a non-zero exit is the gate failing — and it is DATA (recorded
// in ExitCode), never a Go error. This is the headline acceptance for task 3.4:
// "the gate reflects the real exit code."
func TestRunTestFails(t *testing.T) {
	r := Run(context.Background(), t.TempDir(), config.Commands{Test: "exit 7"})
	if r.Passed() {
		t.Errorf("Passed() = true, want false for `exit 7`")
	}
	if !r.Test.Ran {
		t.Errorf("Test.Ran = false, want true (the command ran, it just failed)")
	}
	if r.Test.ExitCode != 7 {
		t.Errorf("Test.ExitCode = %d, want 7 (the real exit code)", r.Test.ExitCode)
	}
	if r.Test.Err != nil {
		t.Errorf("Test.Err = %v, want nil (a non-zero exit is not a harness fault)", r.Test.Err)
	}
}

// TestRunSkipsEmpty: an unset command ("") is skipped, distinct from "ran and
// exited 0" — Ran=false is the discriminant (spec 03 `"" = skip`).
func TestRunSkipsEmpty(t *testing.T) {
	r := Run(context.Background(), t.TempDir(), config.Commands{Test: "", Build: "  ", Lint: ""})
	for _, c := range []CommandResult{r.Build, r.Lint, r.Test} {
		if c.Ran {
			t.Errorf("%s ran, want skipped (empty/whitespace command)", c.Kind)
		}
		if c.Passed() {
			t.Errorf("%s Passed()=true, want false (skipped command is not a pass)", c.Kind)
		}
	}
}

// TestRunBuildLintTestAllRun: every configured command runs and is recorded; with
// all green, both the test gate (Passed) and the strict all-pass (OK) hold.
func TestRunBuildLintTestAllRun(t *testing.T) {
	r := Run(context.Background(), t.TempDir(), config.Commands{
		Build: "exit 0", Lint: "exit 0", Test: "exit 0",
	})
	for _, c := range []CommandResult{r.Build, r.Lint, r.Test} {
		if !c.Ran || !c.Passed() {
			t.Errorf("%s = %+v, want ran+passed", c.Kind, c)
		}
	}
	if !r.Passed() || !r.OK() {
		t.Errorf("Passed()=%v OK()=%v, want both true", r.Passed(), r.OK())
	}
}

// TestPassedIsTestOnly_OKIsStricter pins the spec contract: the done-gate (Passed)
// is the TEST command alone, so a broken build with passing tests still Passes the
// gate — but OK() is false, the signal a caller uses to treat build/lint as
// blocking too. (spec 01 §done-detection names only the test command.)
func TestPassedIsTestOnly_OKIsStricter(t *testing.T) {
	r := Run(context.Background(), t.TempDir(), config.Commands{
		Build: "exit 2", // build fails…
		Test:  "exit 0", // …but tests pass
	})
	if !r.Passed() {
		t.Errorf("Passed() = false, want true (the gate is the test command, which passed)")
	}
	if r.OK() {
		t.Errorf("OK() = true, want false (build exited 2 — the stricter verdict must catch it)")
	}
	if r.Build.ExitCode != 2 {
		t.Errorf("Build.ExitCode = %d, want 2", r.Build.ExitCode)
	}
}

// TestRunCapturesOutput: combined stdout+stderr is captured (for the journal/log
// and future TUI), with stderr folded in alongside stdout.
func TestRunCapturesOutput(t *testing.T) {
	r := Run(context.Background(), t.TempDir(), config.Commands{
		Test: "echo to-stdout; echo to-stderr 1>&2; exit 1",
	})
	if !strings.Contains(r.Test.Output, "to-stdout") || !strings.Contains(r.Test.Output, "to-stderr") {
		t.Errorf("Output missing captured streams: %q", r.Test.Output)
	}
}

// TestRunInWorkingDir: commands run in the given dir, not the harness cwd — so a
// project's test command finds its files where it expects (beside go.mod).
func TestRunInWorkingDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	// `cat marker.txt` only succeeds if cwd is dir.
	r := Run(context.Background(), dir, config.Commands{Test: "cat marker.txt"})
	if !r.Passed() {
		t.Errorf("Passed() = false (cwd not set to project root?); output=%q err=%v", r.Test.Output, r.Test.Err)
	}
	if !strings.Contains(r.Test.Output, "hi") {
		t.Errorf("Output = %q, want it to contain the marker file contents", r.Test.Output)
	}
}

// TestRunContextCancelled: a cancelled context is a harness-side failure surfaced
// in Err — NOT a "tests failed" verdict — so a caller never mistakes shutdown for
// a real test result.
func TestRunContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Run
	r := Run(ctx, t.TempDir(), config.Commands{Test: "sleep 30"})
	if r.Test.Err == nil {
		t.Errorf("Test.Err = nil, want a cancellation error")
	}
	if r.Test.Passed() {
		t.Errorf("Passed() = true, want false on a cancelled run")
	}
}

// TestTail keeps the bound honest: the last maxOutput bytes are retained.
func TestTail(t *testing.T) {
	if got := tail("abcdef", 3); got != "def" {
		t.Errorf("tail(abcdef,3) = %q, want def", got)
	}
	if got := tail("ab", 5); got != "ab" {
		t.Errorf("tail(ab,5) = %q, want ab (already within bound)", got)
	}
}
