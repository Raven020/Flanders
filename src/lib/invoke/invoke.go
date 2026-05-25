// Package invoke composes the argv for a single `claude` CLI invocation from the
// project config and the phase being run. It is the one place that knows how
// Flanders' config maps to `claude` flags, so no consumer hand-assembles argv.
//
// Why a pure builder (no process spawning, no file I/O): keeping composition
// separate from execution makes the exact argv unit-testable (the spec-01/03
// acceptance is "builder emits expected argv per phase/config") and gives the
// process supervisor (plan task 2.5) a single, audited command to launch. The
// builder reads only what it is handed in a Spec plus the validated Config; the
// caller resolves paths and reads the rules file (file I/O stays out of here).
//
// Invariants pinned from the specs (citations in-line below):
//   - Baseline flags are ALWAYS emitted: -p, --output-format stream-json,
//     --verbose, --include-partial-messages (spec 01 §Agent invocation; required
//     by the src/lib/stream parser).
//   - Fresh --session-id every loop, never --resume/--continue (spec 01).
//   - permission_mode "bypassPermissions" → --dangerously-skip-permissions; the
//     other modes → --permission-mode <mode> (spec 01:37, 03 §[agent]).
//   - --max-budget-usd is NEVER emitted: Flanders targets a subscription, which
//     has no per-token dollar cost (spec 00 §Auth, 01 §Agent invocation).
//   - --input-format stream-json only when [agent].stream_input is true (it is the
//     channel the soft wind-down injects through — spec 03 §stream_input, 01
//     §Guardrails). When on, the prompt is delivered over stdin by the supervisor,
//     not as a positional arg.
package invoke

import (
	"crypto/rand"
	"fmt"

	"flanders/src/lib/config"
)

// Spec is the per-invocation input the caller supplies. Everything that varies
// loop-to-loop lives here; everything stable (flags, defaults, model/effort) is
// read from the Config passed to Build.
type Spec struct {
	// Phase selects the agent class via Config.PhaseClass: discuss|plan|build|
	// test|split. Required.
	Phase string

	// SessionID is the fresh per-loop UUID passed as --session-id. Required and
	// non-empty (mint one with NewSessionID). The caller owns it because it must
	// also be recorded in the journal/state for traceability (spec 01
	// §Checkpointing); Build never invents it silently.
	SessionID string

	// SystemPromptAppend is the literal text appended via --append-system-prompt
	// (the loop rules — spec 01 §Agent invocation). The caller reads
	// [agent].rules_file (resolved through src/lib/paths) and passes its contents;
	// Build does no file I/O. Empty → the flag is omitted.
	SystemPromptAppend string

	// Prompt is the composed prompt body for this loop. When the config has
	// stream_input enabled the prompt is delivered over stdin (so it is NOT placed
	// in argv); otherwise it is the trailing positional argument to `claude -p`.
	Prompt string
}

// Command is a ready-to-spawn invocation: the binary plus its arguments, kept
// apart because that is exactly the shape os/exec.CommandContext wants. The
// process supervisor (plan task 2.5) consumes this directly.
type Command struct {
	Bin  string
	Args []string // does NOT include Bin
}

// Argv returns Bin followed by Args — the full argument vector, handy for logging
// and for asserting the whole invocation in tests.
func (c Command) Argv() []string {
	out := make([]string, 0, len(c.Args)+1)
	out = append(out, c.Bin)
	return append(out, c.Args...)
}

// Build composes the claude invocation for spec under cfg. cfg is assumed already
// validated (config.Load validates); Build still returns an error for the inputs
// it alone owns — an empty SessionID, an unknown phase, or an unrecognized
// permission mode — so a misuse fails loudly at compose time rather than spawning
// a malformed command.
func Build(cfg *config.Config, spec Spec) (Command, error) {
	if cfg == nil {
		return Command{}, fmt.Errorf("invoke: nil config")
	}
	if spec.SessionID == "" {
		return Command{}, fmt.Errorf("invoke: SessionID is required (mint one with NewSessionID)")
	}
	class, err := cfg.PhaseClass(spec.Phase)
	if err != nil {
		return Command{}, fmt.Errorf("invoke: %w", err)
	}
	permArgs, err := permissionArgs(cfg.Agent.PermissionMode)
	if err != nil {
		return Command{}, fmt.Errorf("invoke: %w", err)
	}

	args := make([]string, 0, 20)
	// Baseline: non-interactive single turn + parseable event stream (spec 01).
	args = append(args, "-p")
	args = append(args, "--output-format", "stream-json")
	args = append(args, "--verbose")
	args = append(args, "--include-partial-messages")
	// Fresh session every loop (spec 01: no --resume/--continue).
	args = append(args, "--session-id", spec.SessionID)
	// Autonomous operation (spec 01:37, 03 §[agent]).
	args = append(args, permArgs...)
	// Per-phase model/effort (spec 01:41, 07). Defaults are always populated, so
	// these are always emitted.
	args = append(args, "--model", class.Model)
	args = append(args, "--effort", class.Effort)
	// Loop rules as appended system prompt (spec 01:42). Optional.
	if spec.SystemPromptAppend != "" {
		args = append(args, "--append-system-prompt", spec.SystemPromptAppend)
	}
	// stream-json input channel enables the soft wind-down injection (spec 03).
	if cfg.Agent.StreamInput {
		args = append(args, "--input-format", "stream-json")
	} else if spec.Prompt != "" {
		// No stdin channel: the prompt is the trailing positional to `claude -p`.
		args = append(args, spec.Prompt)
	}
	// NB: --max-budget-usd is deliberately never emitted (subscription mode).

	return Command{Bin: cfg.Agent.Bin, Args: args}, nil
}

// permissionArgs maps a config permission_mode to the corresponding claude flag.
// bypassPermissions has a dedicated shortcut flag (--dangerously-skip-permissions,
// the LOCKED default — spec 03); the remaining modes use the generic
// --permission-mode <mode>. config.Validate already restricts the value to these
// four, but Build re-checks so a hand-built Config in a test can't slip through.
func permissionArgs(mode string) ([]string, error) {
	switch mode {
	case "bypassPermissions":
		return []string{"--dangerously-skip-permissions"}, nil
	case "acceptEdits", "default", "plan":
		return []string{"--permission-mode", mode}, nil
	default:
		return nil, fmt.Errorf("unknown permission_mode %q", mode)
	}
}

// NewSessionID mints a fresh RFC-4122 v4 UUID for --session-id. Implemented on
// crypto/rand so the harness needs no external UUID dependency; one fresh id per
// loop is what keeps every iteration a clean-context session (spec 01).
func NewSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("invoke: generate session id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
