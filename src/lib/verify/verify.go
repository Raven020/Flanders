// Package verify is the harness-owned ground-truth gate (specs/01-ralph-loop.md
// §verify, §done-detection). It runs the project's configured [commands] — the
// canonical test command plus the optional build/lint commands — and reports
// their exit codes. The harness, never the agent, decides "done": a loop is
// done only when the canonical test command exits 0 (the agent's self-report is
// advisory). This package is that exit-code check, isolated so the loop driver,
// done-detection (plan task 3.7), and the per-task build flow (4.5) all share one
// source of truth for "did the project pass".
//
// Why a non-zero exit is NOT a Go error. A failing test suite is the normal,
// expected outcome of many loops — it is the very signal that drives the next
// iteration. So [Run] never returns an error for a non-zero exit; it records the
// code in [CommandResult.ExitCode]. A returned-via-Err condition is reserved for
// a genuine harness-side failure (the command could not be started, or ctx was
// cancelled mid-run), which is a different thing entirely and must not be confused
// with "tests failed".
//
// Why sh -c. The [commands] values are shell command lines the user wrote
// verbatim ("go test ./...", "golangci-lint run | tee lint.log"). Running them
// through `sh -c` preserves their shell semantics — pipes, globs, multiple
// arguments — instead of forcing the harness to tokenize them and lose meaning.
package verify

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"

	"flanders/src/lib/config"
)

// maxOutput bounds the combined stdout+stderr captured per command. Test suites
// can emit megabytes; the harness keeps only the tail (where failures and the
// final summary land) so a journal/log line stays cheap and a runaway command
// can't exhaust memory.
const maxOutput = 64 << 10

// Kind names which configured command produced a result. It mirrors the keys of
// the [commands] table in spec 03-config.md.
type Kind string

const (
	KindBuild Kind = "build"
	KindLint  Kind = "lint"
	KindTest  Kind = "test"
)

// CommandResult is the outcome of running one configured command line.
//
// Ran is the key discriminant: a command configured as "" is skipped (spec 03:
// `"" = skip`) and yields Ran=false, distinct from a command that ran and exited
// non-zero (Ran=true, ExitCode!=0). Without Ran the two are indistinguishable,
// since a skipped command also leaves ExitCode at its zero value.
type CommandResult struct {
	Kind     Kind          // which command this is (build|lint|test)
	Command  string        // the shell command line that ran ("" when skipped)
	Ran      bool          // false when the configured command was empty (skipped)
	ExitCode int           // process exit code (0 = pass); -1 on infra failure
	Output   string        // combined stdout+stderr, tail-bounded to maxOutput
	Duration time.Duration // wall-clock for this command
	Err      error         // harness-side failure (couldn't start / ctx cancelled); NOT a non-zero exit
}

// Passed reports the ground-truth pass for this single command: it ran, started
// cleanly, and exited 0. A skipped command (Ran=false) is not "passed" — callers
// that treat "not configured" as acceptable check Ran themselves (see [Result.OK]).
func (r CommandResult) Passed() bool { return r.Ran && r.Err == nil && r.ExitCode == 0 }

// Result aggregates one verify pass over the configured commands.
type Result struct {
	Build CommandResult
	Lint  CommandResult
	Test  CommandResult
}

// Passed is the canonical done-gate (spec 01 §done-detection condition 1): the
// TEST command ran and exited 0. Build and lint are auxiliary signals that Run
// also executes and records, but the spec names only the test command as the
// ground-truth gate — so done-detection keys off this, not [OK].
func (r Result) Passed() bool { return r.Test.Passed() }

// OK is the stricter verdict: every command that actually ran passed. It is the
// signal for a caller that wants build/lint failures to count too (e.g. surfacing
// a broken build before tests are even meaningful). A command configured as ""
// is skipped and does not fail OK. With no commands configured at all, OK is true
// (nothing failed); callers that need a real gate use [Passed] instead.
func (r Result) OK() bool {
	for _, c := range []CommandResult{r.Build, r.Lint, r.Test} {
		if c.Ran && !c.Passed() {
			return false
		}
	}
	return true
}

// Run executes the configured ground-truth commands in dir (the project root) and
// returns their results. Commands run in the order build → lint → test, each
// independently — there is deliberately no fail-fast, so one verify pass records
// the complete picture (a broken lint must not hide the test verdict, and the
// journal/TUI want to show all three). An empty command ("") is skipped.
//
// dir should be the absolute project root so the commands run where the target's
// test harness expects (e.g. beside go.mod). Run takes a context so a long or
// hung test suite can be cancelled by the caller (orchestrator shutdown); a
// cancellation surfaces as CommandResult.Err, not a passed/failed verdict.
func Run(ctx context.Context, dir string, cmds config.Commands) Result {
	return Result{
		Build: runOne(ctx, dir, KindBuild, cmds.Build),
		Lint:  runOne(ctx, dir, KindLint, cmds.Lint),
		Test:  runOne(ctx, dir, KindTest, cmds.Test),
	}
}

// runOne runs a single command line, classifying the result so that a non-zero
// exit (the process ran and failed — an *exec.ExitError) is recorded as ExitCode
// while a start/cancel failure becomes Err. The distinction is the whole point:
// "tests failed" must never look like "the harness broke".
func runOne(ctx context.Context, dir string, kind Kind, cmdline string) CommandResult {
	if strings.TrimSpace(cmdline) == "" {
		return CommandResult{Kind: kind, Ran: false} // "" = skip (spec 03)
	}

	start := time.Now()
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	res := CommandResult{
		Kind:     kind,
		Command:  cmdline,
		Ran:      true,
		Output:   tail(buf.String(), maxOutput),
		Duration: time.Since(start),
	}
	switch {
	case err == nil:
		res.ExitCode = 0
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// The process ran and exited non-zero (or was signalled → ExitCode -1).
			// This is the normal "tests failed" path, not a harness fault.
			res.ExitCode = exitErr.ExitCode()
		} else {
			// Couldn't start the process, or ctx was cancelled — a harness-side
			// failure. -1 keeps it out of the "exited 0" pass set even if read raw.
			res.Err = err
			res.ExitCode = -1
		}
	}
	return res
}

// tail returns the last max bytes of s (where a failing command's error and final
// summary land), or all of s when it is already within the bound.
func tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}
