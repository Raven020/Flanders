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
	"strings"

	"flanders/src/lib/config"
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
// path; every returned field is absolute. It is NewFromConfig with no overlay —
// the right constructor when no config has been loaded yet (e.g. `flanders init`,
// which writes the default config before any exists to read).
func New(root string) (*Paths, error) {
	return NewFromConfig(root, nil)
}

// NewFromConfig resolves the layout against root, overlaying any non-empty
// configurable location from cfg onto the documented defaults. This is the
// single point where spec-03 [paths] (specs/tasks/journal/plan/state) and
// [agent].rules_file actually take effect: before it existed, paths.New
// hardcoded the Default* constants and ignored config entirely, so the whole
// [paths] section was a silent no-op (spec-03 non-compliance). A nil cfg, or
// empty fields within it, keep the defaults — so absent keys never override.
//
// Configurable locations may be relative (resolved against root, the documented
// shape) or absolute (used as-is). The config file's own location, the diagnostic
// log, and the .flanders/ working dir are NOT configurable — they are fixed under
// root so the harness can always find where it loaded its config from.
func NewFromConfig(root string, cfg *config.Config) (*Paths, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root %q: %w", root, err)
	}
	// resolve turns a config-relative location into an absolute path; an already
	// absolute location is taken verbatim (a user may point [paths] outside root).
	resolve := func(loc string) string {
		if filepath.IsAbs(loc) {
			return filepath.Clean(loc)
		}
		return filepath.Join(abs, filepath.FromSlash(loc))
	}

	specs, tasks := DefaultSpecs, DefaultTasks
	journal, plan, state := DefaultJournal, DefaultPlan, DefaultState
	rules := DefaultRules
	if cfg != nil {
		specs = overlay(specs, cfg.Paths.Specs)
		tasks = overlay(tasks, cfg.Paths.Tasks)
		journal = overlay(journal, cfg.Paths.Journal)
		plan = overlay(plan, cfg.Paths.Plan)
		state = overlay(state, cfg.Paths.State)
		rules = overlay(rules, cfg.Agent.RulesFile)
	}

	return &Paths{
		Root:     abs,
		Specs:    resolve(specs),
		Tasks:    resolve(tasks),
		Journal:  resolve(journal),
		Plan:     resolve(plan),
		State:    resolve(state),
		Rules:    resolve(rules),
		Config:   resolve(DefaultConfig),
		Log:      resolve(DefaultLog),
		Flanders: resolve(flandersDir),
	}, nil
}

// overlay returns val when it carries a non-blank location, else the default.
// Whitespace-only config values are treated as absent so a stray "  " in the
// file can't blank out a default location.
func overlay(def, val string) string {
	if strings.TrimSpace(val) != "" {
		return val
	}
	return def
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
