// Package supervise spawns the `claude` CLI, streams and folds its stdout into a
// typed [stream.LoopObservation], captures stderr, enforces the per-iteration
// wall-clock timeout (killing the whole process group), and exposes the stdin
// channel the soft wind-down injects through. It is the process-lifecycle half of
// the loop's "spawn → observe" steps (specs/01 §Iteration anatomy, §Guardrails);
// the pure argv composition lives in src/lib/invoke and the wire decoding in
// src/lib/stream — this package only orchestrates the OS process around them.
//
// Why a handle (Proc) plus a Run convenience:
//   - Most loops just want Run(ctx, spec): spawn, stream to completion, return the
//     folded Result (which the loop driver classifies with
//     [stream.LoopObservation.Classify], passing Result.ExitCode).
//   - The context-pressure guardrail (plan task 3.11) needs to inject a "wrap up"
//     message mid-loop and, failing that, hard-kill. Start returns a live Proc with
//     Inject/Kill/CloseInput for exactly that, while folding stays internal so the
//     stream is still decoded exactly once (specs/08 "no ad-hoc re-parsing").
//
// Un-missable stdin contract: when [agent].stream_input is on, invoke.Build drops
// the prompt from argv because it must travel over stdin (invoke.go) — with no
// compile-time enforcement there. This package OWNS that write: Start requires a
// non-empty Spec.Prompt whenever Spec.StreamInput is set and writes it itself, so a
// forgotten write can never spawn an empty turn (plan task 2.5 audit note).
package supervise

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"flanders/src/lib/invoke"
	"flanders/src/lib/stream"
)

// killGrace bounds how long Wait waits for the process and its pipes to wind down
// after the timeout/cancel fires the group kill, so Wait can never hang on a child
// that ignores the signal (exec.Cmd.WaitDelay).
const killGrace = 5 * time.Second

// maxStderr caps captured stderr so a chatty or looping child cannot exhaust
// memory over a multi-day unattended run; the tail past the cap is dropped.
const maxStderr = 256 << 10 // 256 KiB

// Spec is everything a single supervised invocation needs. Command comes straight
// from invoke.Build; the rest the loop driver maps from config and the current loop.
type Spec struct {
	// Command is the ready-to-spawn invocation from invoke.Build (Bin + Args).
	Command invoke.Command

	// Prompt is the composed prompt body. REQUIRED when StreamInput is true (it is
	// written to stdin here); ignored otherwise (invoke.Build already placed it in
	// Command.Args as the trailing positional).
	Prompt string

	// StreamInput mirrors [agent].stream_input. When true the prompt — and any
	// later Inject — travel over the CLI's stdin (`--input-format stream-json`),
	// which is the channel the soft wind-down uses. Must match how Command was built.
	StreamInput bool

	// Timeout is the per-iteration wall-clock cap ([guardrails].iteration_timeout).
	// On expiry the process group is killed and Result.TimedOut is set. Zero means
	// no timeout (the parent ctx still governs cancellation).
	Timeout time.Duration

	// RawSink, if non-nil, receives the verbatim stdout bytes as they stream — the
	// raw transcript the journal archives (specs/01 §journal). It is teed off the
	// same read the decoder consumes, so it captures every byte including lines the
	// decoder skips as unparseable (a faithful archive).
	RawSink io.Writer

	// OnEvent, if non-nil, is called for every decoded event in the read goroutine,
	// before it is folded. It receives the live Proc so a guardrail can drive a
	// [stream.Tracker] off the events and then Inject/Kill the same process. The
	// hook must not block (it is on the decode path).
	OnEvent func(p *Proc, ev *stream.Event)

	// Log receives diagnostics (skipped lines, kill events). Nil discards them.
	Log *slog.Logger
}

// Result is the outcome of a supervised invocation. ExitCode plus the Observation
// are exactly what [stream.LoopObservation.Classify] needs to decide success vs.
// usage-limit vs. error.
type Result struct {
	Observation *stream.LoopObservation // folded stdout (never nil once Wait returns)
	ExitCode    int                     // process exit code; -1 if killed by a signal
	TimedOut    bool                    // killed because Spec.Timeout elapsed
	Canceled    bool                    // killed because the parent ctx was canceled
	Stderr      string                  // captured stderr (bounded by maxStderr)
	Duration    time.Duration           // wall-clock from spawn to reap
	StreamErr   error                   // non-EOF error reading/decoding stdout, if any
}

// Proc is a running supervised process. Inject/Kill/CloseInput are safe to call
// from any goroutine (including from the OnEvent hook). Call Wait exactly once to
// reap the process and collect the Result.
type Proc struct {
	log       *slog.Logger
	cmd       *exec.Cmd
	cancel    context.CancelFunc // non-nil only when Spec.Timeout > 0
	parentCtx context.Context
	runCtx    context.Context
	started   time.Time

	streamDone chan struct{} // closed when stdout is fully decoded+folded
	stderrDone chan struct{} // closed when stderr is fully drained
	obs        *stream.LoopObservation
	streamErr  error
	stderr     *boundedBuffer

	mu        sync.Mutex
	stdin     io.WriteCloser
	stdinOpen bool
}

// Run is the blocking convenience: Start then Wait. It is what a loop with no
// mid-flight injection (every loop until the guardrail trips) uses.
func Run(ctx context.Context, spec Spec) (*Result, error) {
	p, err := Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	return p.Wait(), nil
}

// Start spawns the process and begins streaming stdout/stderr in background
// goroutines. It returns once the process is started (and, for StreamInput, once
// the initial prompt has been written to stdin). Errors only for misuse it can
// detect up front (empty command, missing prompt) or a spawn failure.
func Start(ctx context.Context, spec Spec) (*Proc, error) {
	if spec.Log == nil {
		spec.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if spec.Command.Bin == "" {
		return nil, errors.New("supervise: empty command (build it with invoke.Build first)")
	}
	// Un-missable stdin contract (see package doc): an empty prompt over stdin would
	// be an empty turn, so reject it at the door rather than silently spawning one.
	if spec.StreamInput && spec.Prompt == "" {
		return nil, errors.New("supervise: stream_input is set but Prompt is empty (it must be written to stdin)")
	}

	// runCtx bounds the run by Timeout (when set) under the caller's ctx. exec wires
	// cmd.Cancel to fire when runCtx is done; Cancel kills the whole process group.
	runCtx := ctx
	var cancel context.CancelFunc
	if spec.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
	}

	cmd := exec.CommandContext(runCtx, spec.Command.Bin, spec.Command.Args...)
	setpgid(cmd) // own process group so the kill reaches CLI-spawned subprocesses
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = killGrace

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stopTimer(cancel)
		return nil, fmt.Errorf("supervise: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		stopTimer(cancel)
		return nil, fmt.Errorf("supervise: stderr pipe: %w", err)
	}
	var stdin io.WriteCloser
	if spec.StreamInput {
		stdin, err = cmd.StdinPipe()
		if err != nil {
			stopTimer(cancel)
			return nil, fmt.Errorf("supervise: stdin pipe: %w", err)
		}
	}

	if err := cmd.Start(); err != nil {
		stopTimer(cancel)
		return nil, fmt.Errorf("supervise: start %q: %w", spec.Command.Bin, err)
	}

	p := &Proc{
		log:        spec.Log,
		cmd:        cmd,
		cancel:     cancel,
		parentCtx:  ctx,
		runCtx:     runCtx,
		started:    time.Now(),
		streamDone: make(chan struct{}),
		stderrDone: make(chan struct{}),
		stderr:     newBoundedBuffer(maxStderr),
		stdin:      stdin,
		stdinOpen:  stdin != nil,
	}

	// Write the initial prompt synchronously so it is always the first thing on
	// stdin (deterministic ordering relative to any later Inject). The OS pipe
	// buffers ~64 KiB, covering typical prompts without blocking before the CLI reads.
	if spec.StreamInput {
		if err := p.Inject(spec.Prompt); err != nil {
			p.log.Warn("supervise: writing initial prompt to stdin failed", "err", err)
		}
	}

	// Drain stderr into a bounded buffer.
	go func() {
		defer close(p.stderrDone)
		_, _ = io.Copy(p.stderr, stderrPipe)
	}()

	// Decode + fold stdout exactly once (specs/08). Tee the verbatim bytes to the
	// journal sink first so the archive is byte-faithful even for skipped lines.
	go func() {
		defer close(p.streamDone)
		var r io.Reader = stdout
		if spec.RawSink != nil {
			r = io.TeeReader(stdout, spec.RawSink)
		}
		obs, ferr := stream.ObserveFunc(r, p.log, func(ev *stream.Event) {
			if spec.OnEvent != nil {
				spec.OnEvent(p, ev)
			}
		})
		p.obs = obs
		p.streamErr = ferr
	}()

	return p, nil
}

// Inject writes text to the running session's stdin as a stream-json user message
// (the soft wind-down / discuss channel). It errors when StreamInput is off (no
// stdin) or after the input has been closed. Safe for concurrent use; writes are
// serialized so an injected line never interleaves with the initial prompt.
func (p *Proc) Inject(text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin == nil {
		return errors.New("supervise: cannot inject — stream_input is off (no stdin)")
	}
	if !p.stdinOpen {
		return errors.New("supervise: cannot inject — stdin is closed")
	}
	line, err := stream.EncodeUserMessage(text)
	if err != nil {
		return err
	}
	if _, err := p.stdin.Write(line); err != nil {
		return fmt.Errorf("supervise: write stdin: %w", err)
	}
	return nil
}

// CloseInput closes the session's stdin, signalling end-of-input to a CLI that
// reads to EOF. Idempotent and a no-op when StreamInput is off. Wait calls it for
// the caller, so an explicit call is only needed to end input early.
func (p *Proc) CloseInput() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin == nil || !p.stdinOpen {
		return nil
	}
	p.stdinOpen = false
	return p.stdin.Close()
}

// Kill terminates the process group immediately (the hard backstop, specs/01
// §Guardrails tier 3) and closes stdin. Wait still returns the (partial) Result.
func (p *Proc) Kill() {
	if err := killGroup(p.cmd); err != nil {
		p.log.Warn("supervise: kill failed", "err", err)
	}
	_ = p.CloseInput()
}

// Wait blocks until the process exits (or is killed by timeout/cancel), then
// returns the folded Result. It must be called exactly once.
func (p *Proc) Wait() *Result {
	// Close stdin first so a CLI that reads to EOF (and the cat-style stub the tests
	// use) can finish and close stdout — otherwise waiting on streamDone below would
	// deadlock against a child still waiting for input. Injection happens before Wait.
	_ = p.CloseInput()

	// Drain both pipes fully BEFORE reaping: os/exec closes the pipes inside Wait,
	// and it is incorrect to call Wait before all reads complete.
	<-p.streamDone
	<-p.stderrDone

	waitErr := p.cmd.Wait()

	// Read the context state before releasing the timer, so DeadlineExceeded is not
	// masked by our own cancel().
	timedOut := errors.Is(p.runCtx.Err(), context.DeadlineExceeded) && p.parentCtx.Err() == nil
	canceled := p.parentCtx.Err() != nil
	stopTimer(p.cancel)

	obs := p.obs
	if obs == nil {
		obs = &stream.LoopObservation{} // never hand back a nil observation
	}
	return &Result{
		Observation: obs,
		ExitCode:    exitCode(waitErr),
		TimedOut:    timedOut,
		Canceled:    canceled,
		Stderr:      p.stderr.String(),
		Duration:    time.Since(p.started),
		StreamErr:   p.streamErr,
	}
}

// exitCode maps cmd.Wait's error to a process exit code: 0 on clean exit, the
// reported code for an ExitError (which is -1 when the process was killed by a
// signal — e.g. our timeout SIGKILL), and -1 for any other wait failure.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func stopTimer(cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
}

// boundedBuffer is an io.Writer that retains at most max bytes (the head) and
// silently drops the rest, while always reporting a full write so io.Copy keeps
// draining the pipe to EOF.
type boundedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func newBoundedBuffer(max int) *boundedBuffer { return &boundedBuffer{max: max} }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if rem := b.max - b.buf.Len(); rem > 0 {
		if len(p) > rem {
			b.buf.Write(p[:rem])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
