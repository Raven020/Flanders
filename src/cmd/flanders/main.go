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

	"flanders/src/lib/logging"
	"flanders/src/lib/paths"
)

// Version is the harness version, bumped on each green build (PROMPT rule:
// start at 0.0.0 and increment patch).
const Version = "0.0.1"

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
	return nil
}
