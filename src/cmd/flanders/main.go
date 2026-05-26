// Command flanders wraps the claude CLI and drives a Ralph loop until the work
// is provably complete (all tasks done, real tests green). See specs/00-overview.md.
//
// Today the command surface is: `flanders init` (write a commented default
// config) and the bare invocation (the orchestrate run: locate the project root,
// ensure .flanders/, load the run-state cache and journal, then drive the
// plan→build pipeline to completion via src/lib/orchestrate). The remaining surface
// (discuss|plan|build sub-commands) lands in later phases — see IMPLEMENTATION_PLAN.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"flanders/src/lib/config"
	"flanders/src/lib/journal"
	"flanders/src/lib/logging"
	"flanders/src/lib/loop"
	"flanders/src/lib/orchestrate"
	"flanders/src/lib/paths"
	"flanders/src/lib/rules"
	"flanders/src/lib/state"
	"flanders/src/lib/task"
)

// Version is the harness version, bumped on each green build (PROMPT rule:
// start at 0.0.0 and increment patch).
const Version = "0.0.28"

const usage = `usage: flanders [command]

commands:
  init      write a commented default .flanders/config.toml (never overwrites)
  (bare)    orchestrate plan → build until the plan is complete and green

forthcoming (later phases): discuss, plan, build`

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "flanders: %v\n", err)
		os.Exit(1)
	}
}

// dispatch routes the command word to its handler. Keeping this a thin, pure
// switch (no globals) makes it testable and gives the not-yet-built commands an
// honest message instead of silently doing the wrong thing.
func dispatch(args []string) error {
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "init":
		return runInit(args[1:])
	case "":
		return runOrchestrate()
	case "discuss", "plan", "build":
		return fmt.Errorf("command %q is not implemented yet — see IMPLEMENTATION_PLAN.md", cmd)
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
}

// runInit writes a commented default config.toml when the project has none
// (spec 03-config.md). It resolves the project root the same way the orchestrate
// startup does, so `flanders init` from anywhere inside a project writes to that
// project's .flanders/.
func runInit(_ []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := paths.FindRoot(cwd)
	if err != nil {
		root = cwd // not inside a project yet: init the current directory
	}
	return initAt(root, os.Stdout)
}

// initAt writes the default config under root's .flanders/ and reports the
// outcome to w. It is factored out of runInit so tests can drive it against a
// temp directory without changing the process working directory.
func initAt(root string, w io.Writer) error {
	p, err := paths.New(root)
	if err != nil {
		return err
	}
	if err := p.EnsureFlanders(); err != nil {
		return err
	}
	wrote, err := config.WriteDefault(p.Config)
	if err != nil {
		return err
	}
	if wrote {
		fmt.Fprintf(w, "flanders: wrote default config to %s\n", p.Config)
	} else {
		fmt.Fprintf(w, "flanders: config already exists at %s (not overwriting)\n", p.Config)
	}

	// Materialize the loop rules alongside the config so the user can read and tune
	// the agent's behavioral contract (specs/01 §invocation). Like the config, this
	// never overwrites an existing file; and the loop falls back to the same built-in
	// default when the file is absent, so the rules are always in force regardless.
	wroteRules, err := rules.WriteDefault(p.Rules)
	if err != nil {
		return err
	}
	if wroteRules {
		fmt.Fprintf(w, "flanders: wrote default loop rules to %s\n", p.Rules)
	} else {
		fmt.Fprintf(w, "flanders: loop rules already exist at %s (not overwriting)\n", p.Rules)
	}
	return nil
}

// runOrchestrate is the bare-`flanders` startup: locate the project root, ensure
// the .flanders/ working directory, start the file-backed logger, then load the
// run-state cache and journal. The orchestrate loop itself (plan→build) lands in
// Phase 5.
func runOrchestrate() error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	// Locate the project root by walking up for a marker; if we are not inside
	// a project yet, operate from the current directory.
	root, err := paths.FindRoot(cwd)
	if err != nil {
		root = cwd
	}
	// Load config FIRST so every location resolves through it (spec 03 [paths]).
	// This is the single startup config-load all later phases depend on; without
	// it the [paths] section was parsed and then ignored (a silent no-op).
	cfg, err := loadConfigOrDefault(root)
	if err != nil {
		return err
	}
	p, err := paths.NewFromConfig(root, cfg)
	if err != nil {
		return err
	}
	if err := p.EnsureFlanders(); err != nil {
		return err
	}
	// Log level is not configurable yet: spec 03 has no [logging] section, so a
	// config field would be speculative. Left at Info; revisit with the [tui]/
	// [logging] config-section pass (IMPLEMENTATION_PLAN.md findings 14/15).
	log, err := logging.New(p.Log, slog.LevelInfo)
	if err != nil {
		return err
	}
	defer log.Close()

	log.Info("flanders starting", "version", Version, "root", p.Root)
	fmt.Printf("flanders %s (project root: %s)\n", Version, p.Root)

	// Load the run-state cache on startup (spec 09). A missing tasks dir yields an
	// empty store (the expected pre-plan state); a missing or corrupt state.json is
	// a cache miss, not an error — LoadOrRebuild reconstructs it from the task store
	// (ground truth). The fallback phase is `orchestrate`: bare `flanders` drives
	// plan→build, and the command surface that overrides this lands in Phase 8.
	store, err := task.LoadDir(p.Tasks)
	if err != nil {
		return fmt.Errorf("load tasks: %w", err)
	}
	st, rebuilt, err := state.LoadOrRebuild(p.State, store, state.PhaseOrchestrate)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	log.Info("run state loaded", "rebuilt", rebuilt, "phase", st.Phase,
		"run_state", st.RunState, "current_task", st.CurrentTask)

	// Open the per-loop journal (spec 01 §journal). It is the tier-2 history the
	// loop driver (Phase 3) appends to and the TUI renders; opening it here makes
	// the directory ready and surfaces how many loops this project has on record —
	// the depth the orchestrator will fold into a rebuilt state.Iter (the
	// journal-tier enrichment state.Rebuild defers to this tier, spec 09).
	jrnl, err := journal.Open(p.Journal)
	if err != nil {
		return fmt.Errorf("open journal: %w", err)
	}
	depth, err := jrnl.Len()
	if err != nil {
		return fmt.Errorf("read journal: %w", err)
	}
	log.Info("journal opened", "dir", jrnl.Dir(), "entries", depth)

	// Build the loop engine and the orchestrator, then run the full plan→build
	// pipeline (spec 06). The orchestrator owns the run-state machine and persists
	// state.json at every transition, so a Ctrl-C (ctx cancel) is a graceful stop the
	// next run resumes from — not a crash.
	drv, err := loop.New(loop.Options{Config: cfg, Paths: p, Journal: jrnl, Log: log.Logger})
	if err != nil {
		return fmt.Errorf("build loop driver: %w", err)
	}
	orch, err := orchestrate.New(orchestrate.Options{
		Driver:    drv,
		Config:    cfg,
		Paths:     p,
		State:     st,
		StatePath: p.State,
		Log:       log.Logger,
	})
	if err != nil {
		return fmt.Errorf("build orchestrator: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sum, err := orch.Run(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Println("flanders: interrupted — state saved; re-run to resume.")
			log.Info("run interrupted; state persisted for resume")
			return nil
		}
		return fmt.Errorf("orchestrate: %w", err)
	}
	printSummary(sum)
	log.Info("run finished", "run_state", sum.RunState, "tasks_done", sum.TasksDone,
		"plan_iters", sum.PlanIters, "build_iters", sum.BuildIters)
	return nil
}

// printSummary writes the terminal report (spec 06 §Termination & handoff) to stdout.
func printSummary(s *orchestrate.Summary) {
	switch s.RunState {
	case state.StateDone:
		fmt.Println("\nflanders: ✓ DONE — all tasks complete and the test gate is green.")
	case state.StateHalted:
		fmt.Printf("\nflanders: ⚠ HALTED — %s\n", s.HaltReason)
		if s.HaltTask != "" {
			fmt.Printf("          (task %s)\n", s.HaltTask)
		}
	default:
		fmt.Printf("\nflanders: run ended in state %s\n", s.RunState)
	}
	fmt.Printf("  tasks: %d done", s.TasksDone)
	if s.TasksBlocked > 0 {
		fmt.Printf(", %d blocked", s.TasksBlocked)
	}
	fmt.Printf("\n  iterations: %d plan + %d build = %d total\n", s.PlanIters, s.BuildIters, s.TotalIters)
	fmt.Printf("  duration: %s · cost: $%.4f\n", s.Duration.Round(time.Second), s.CostUSD)
}

// loadConfigOrDefault loads .flanders/config.toml from root, falling back to the
// documented defaults when the file is absent. A missing config is normal before
// `flanders init`, so a bare `flanders` must still run on defaults; but a config
// that exists yet fails to parse or validate is a HARD error — the user asked for
// something specific and we must not silently ignore it and run on defaults.
//
// The config file's own location is not configurable, so it is resolved with the
// default layout (paths.New); only after loading do we build the overlaid Paths.
func loadConfigOrDefault(root string) (*config.Config, error) {
	dp, err := paths.New(root)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(dp.Config)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			def := config.Default()
			return &def, nil
		}
		return nil, err
	}
	return cfg, nil
}
