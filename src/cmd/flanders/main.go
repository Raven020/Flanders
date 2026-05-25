// Command flanders wraps the claude CLI and drives a Ralph loop until the work
// is provably complete (all tasks done, real tests green). See specs/00-overview.md.
//
// This is the foundation entry point: it locates the project root, ensures the
// .flanders/ working directory exists, and starts the file-backed logger. The
// full command surface (discuss|plan|build|init and bare orchestrate) lands in
// later phases — see IMPLEMENTATION_PLAN.md.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"flanders/src/lib/journal"
	"flanders/src/lib/logging"
	"flanders/src/lib/paths"
	"flanders/src/lib/state"
	"flanders/src/lib/task"
)

// Version is the harness version, bumped on each green build (PROMPT rule:
// start at 0.0.0 and increment patch).
const Version = "0.0.6"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "flanders: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
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
	p, err := paths.New(root)
	if err != nil {
		return err
	}
	if err := p.EnsureFlanders(); err != nil {
		return err
	}
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
	return nil
}
