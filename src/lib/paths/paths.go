// Package paths resolves Flanders's on-disk layout relative to a project root.
//
// Why a dedicated helper: every other package (config, journal, state, plan)
// needs the same set of locations, and specs/03-config.md makes the [paths]
// configurable. Centralizing resolution here keeps a single source of truth —
// no consumer hand-joins ".flanders/..." on its own, so the layout can change
// in exactly one place.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// Default locations, relative to the project root. These mirror the [paths]
// defaults in specs/03-config.md, plus the agent rules/config/log files that
// live alongside them under .flanders/.
const (
	DefaultSpecs   = "specs"                  // user-authored specs (planning input)
	DefaultTasks   = "specs/tasks"            // generated task files
	DefaultJournal = ".flanders/journal"      // per-iteration logs
	DefaultPlan    = "IMPLEMENTATION_PLAN.md" // derived human-readable checklist
	DefaultState   = ".flanders/state.json"   // resumable cursor / run-state cache
	DefaultRules   = ".flanders/rules.md"     // appended via --append-system-prompt
	DefaultConfig  = ".flanders/config.toml"  // harness configuration
	DefaultLog     = ".flanders/flanders.log" // file-backed diagnostic log
	flandersDir    = ".flanders"
)

// Paths holds absolute, resolved locations for the project's artifacts. All
// fields are absolute so consumers never re-resolve against a working dir.
type Paths struct {
	Root     string // project root (absolute)
	Specs    string
	Tasks    string
	Journal  string
	Plan     string
	State    string
	Rules    string
	Config   string
	Log      string
	Flanders string // the .flanders/ working directory
}

// New resolves the default layout against root. root is cleaned to an absolute
// path; every returned field is absolute.
func New(root string) (*Paths, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root %q: %w", root, err)
	}
	join := func(rel string) string { return filepath.Join(abs, filepath.FromSlash(rel)) }
	return &Paths{
		Root:     abs,
		Specs:    join(DefaultSpecs),
		Tasks:    join(DefaultTasks),
		Journal:  join(DefaultJournal),
		Plan:     join(DefaultPlan),
		State:    join(DefaultState),
		Rules:    join(DefaultRules),
		Config:   join(DefaultConfig),
		Log:      join(DefaultLog),
		Flanders: join(flandersDir),
	}, nil
}

// EnsureFlanders creates the .flanders/ directory and its journal subdirectory
// on demand. Idempotent — safe to call on every startup.
func (p *Paths) EnsureFlanders() error {
	for _, dir := range []string{p.Flanders, p.Journal} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %q: %w", dir, err)
		}
	}
	return nil
}

// rootMarkers identify a project root when walking up from a start directory.
// .flanders means Flanders has already run here; .git / specs identify a fresh
// target project the harness was launched inside.
var rootMarkers = []string{flandersDir, ".git", DefaultSpecs}

// FindRoot walks up from start (inclusive) looking for a project marker. It
// returns the first ancestor directory containing one, or an error if none is
// found before the filesystem root. Callers typically fall back to the working
// directory when this errors.
func FindRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolve start %q: %w", start, err)
	}
	for {
		for _, marker := range rootMarkers {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no project root (looked for %v) at or above %q", rootMarkers, start)
		}
		dir = parent
	}
}
