// Package config loads and validates Flanders' per-project configuration from
// .flanders/config.toml.
//
// Why TOML and why this shape: spec 03-config.md locks TOML as the config
// format (sectioned, idiomatic in Go, less whitespace-fragile than YAML; task
// files stay YAML by design). The schema here mirrors that spec section for
// section so the file is the single source of truth and consumers never re-parse
// raw TOML themselves.
//
// Default-then-overlay is the core mechanic: Load starts from Default() — a
// fully-populated struct — and decodes the user's file on top of it. TOML
// decoding only touches keys that are present, so absent keys keep their
// documented defaults and present keys win. The one field with no default is
// [commands].test: the build phase REQUIRES it (spec 03 §"Build phase requires
// [commands].test"), so it stays empty until the user sets it and is enforced by
// ValidateForBuild rather than silently defaulted (a default would make "missing"
// undetectable).
package config

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration wraps time.Duration so duration-valued config fields (written as TOML
// strings like "20m") decode via encoding.TextUnmarshaler into a real
// time.Duration that consumers can use directly — no re-parsing at every call
// site.
type Duration struct {
	time.Duration
}

// UnmarshalText parses a Go duration string (e.g. "20m", "1h30m") into d.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(strings.TrimSpace(string(text)))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", string(text), err)
	}
	d.Duration = parsed
	return nil
}

// MarshalText renders d back to a Go duration string, so a loaded config can be
// re-emitted (e.g. by `flanders init`) without losing the value.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.Duration.String()), nil
}

// Config is the typed, fully-resolved configuration. Every section maps 1:1 to a
// table in spec 03-config.md.
type Config struct {
	Paths      Paths      `toml:"paths"`
	Commands   Commands   `toml:"commands"`
	Agent      Agent      `toml:"agent"`
	Phases     Phases     `toml:"phases"`
	Subagents  Subagents  `toml:"subagents"`
	Context    Context    `toml:"context"`
	Guardrails Guardrails `toml:"guardrails"`
	Usage      Usage      `toml:"usage"`
	Git        Git        `toml:"git"`
}

// Paths holds project-root-relative locations ([paths]). These are the raw
// strings from config; resolving them to absolute paths is the job of
// src/lib/paths.
type Paths struct {
	Specs   string `toml:"specs"`
	Tasks   string `toml:"tasks"`
	Journal string `toml:"journal"`
	Plan    string `toml:"plan"`
	State   string `toml:"state"`
}

// Commands are the project's ground-truth commands ([commands]). Test is the
// done-gate and is required for the build phase; Build and Lint are optional
// ("" disables).
type Commands struct {
	Test  string `toml:"test"`
	Build string `toml:"build"`
	Lint  string `toml:"lint"`
}

// Agent controls how the claude CLI is invoked ([agent]).
type Agent struct {
	Bin            string `toml:"bin"`
	PermissionMode string `toml:"permission_mode"`
	RulesFile      string `toml:"rules_file"`
	StreamInput    bool   `toml:"stream_input"`
}

// AgentClass is a model+effort pair for a phase agent or a subagent class.
type AgentClass struct {
	Model  string `toml:"model"`
	Effort string `toml:"effort"`
}

// Phases holds the per-phase lead-agent settings ([phases.*]). split is not its
// own class — it reuses Plan (spec 07-agents-and-models.md).
type Phases struct {
	Discuss AgentClass `toml:"discuss"`
	Plan    AgentClass `toml:"plan"`
	Build   AgentClass `toml:"build"`
	Test    AgentClass `toml:"test"`
}

// Subagents holds the default model+effort for agents a lead spins up, plus
// optional per-class overrides under [subagents.<name>] (e.g. [subagents.explore]).
//
// The mixed shape — flat model/effort keys alongside named sub-tables — does not
// map to a plain struct, so Subagents implements toml.Unmarshaler to split the
// two apart. Per-class overrides are OPEN for v1 (spec 03/07) but parsing them
// here keeps the loader forward-compatible.
type Subagents struct {
	Model   string                `toml:"model"`
	Effort  string                `toml:"effort"`
	Classes map[string]AgentClass `toml:"-"`
}

// UnmarshalTOML splits the [subagents] table into the flat model/effort defaults
// and per-class overrides. Fields absent from the file are left untouched so
// pre-set defaults survive (default-then-overlay).
func (s *Subagents) UnmarshalTOML(data any) error {
	table, ok := data.(map[string]any)
	if !ok {
		return fmt.Errorf("[subagents] must be a table, got %T", data)
	}
	for key, raw := range table {
		switch key {
		case "model":
			str, ok := raw.(string)
			if !ok {
				return fmt.Errorf("[subagents].model must be a string, got %T", raw)
			}
			s.Model = str
		case "effort":
			str, ok := raw.(string)
			if !ok {
				return fmt.Errorf("[subagents].effort must be a string, got %T", raw)
			}
			s.Effort = str
		default:
			sub, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("[subagents.%s] must be a table, got %T", key, raw)
			}
			class, err := agentClassFromMap(key, sub)
			if err != nil {
				return err
			}
			if s.Classes == nil {
				s.Classes = make(map[string]AgentClass)
			}
			s.Classes[key] = class
		}
	}
	return nil
}

// agentClassFromMap reads model/effort out of a decoded sub-table. Missing keys
// yield empty strings (the override simply doesn't set them).
func agentClassFromMap(name string, sub map[string]any) (AgentClass, error) {
	var class AgentClass
	if v, ok := sub["model"]; ok {
		str, ok := v.(string)
		if !ok {
			return class, fmt.Errorf("[subagents.%s].model must be a string, got %T", name, v)
		}
		class.Model = str
	}
	if v, ok := sub["effort"]; ok {
		str, ok := v.(string)
		if !ok {
			return class, fmt.Errorf("[subagents.%s].effort must be a string, got %T", name, v)
		}
		class.Effort = str
	}
	return class, nil
}

// Context holds context-pressure thresholds ([context]). WindowTokens of 0 means
// "auto-detect by model" (table is OPEN in spec 03); SoftPct/HardPct are
// fractions of the window that trip the soft wind-down and hard backstop.
type Context struct {
	WindowTokens int     `toml:"window_tokens"`
	SoftPct      float64 `toml:"soft_pct"`
	HardPct      float64 `toml:"hard_pct"`
}

// Guardrails holds the loop guardrail limits ([guardrails]).
type Guardrails struct {
	MaxIterations    int      `toml:"max_iterations"`
	StallN           int      `toml:"stall_n"`
	IterationTimeout Duration `toml:"iteration_timeout"`
}

// Usage controls subscription usage-window handling ([usage]). This is NOT a
// dollar budget (spec 00) — OnLimit decides whether to wait+auto-resume or halt;
// MaxCycles of 0 drains unlimited windows; Backoff is the fallback wait when a
// reset time isn't parseable.
type Usage struct {
	OnLimit   string   `toml:"on_limit"`
	MaxCycles int      `toml:"max_cycles"`
	Backoff   Duration `toml:"backoff"`
}

// Git controls checkpointing ([git]).
type Git struct {
	Enabled       bool   `toml:"enabled"`
	InitIfMissing bool   `toml:"init_if_missing"`
	CommitEach    string `toml:"commit_each"`
	MessageTmpl   string `toml:"message_tmpl"`
}

// Valid value sets for enum-like fields. Kept here so Validate and any future
// `init` writer share one source of truth.
var (
	validEfforts        = []string{"low", "medium", "high", "xhigh", "max"}
	validOnLimit        = []string{"wait", "halt"}
	validCommitEach     = []string{"progress", "iteration", "off"}
	validPermissionMode = []string{"bypassPermissions", "acceptEdits", "default", "plan"}
)

// Default returns a Config populated with every documented default from spec
// 03-config.md, EXCEPT [commands].test which is intentionally left empty (it has
// no default and is required for the build phase; see package doc).
func Default() Config {
	return Config{
		Paths: Paths{
			Specs:   "specs",
			Tasks:   "specs/tasks",
			Journal: ".flanders/journal",
			Plan:    "IMPLEMENTATION_PLAN.md",
			State:   ".flanders/state.json",
		},
		Commands: Commands{
			Test:  "", // REQUIRED for build; no default by design.
			Build: "",
			Lint:  "",
		},
		Agent: Agent{
			Bin:            "claude",
			PermissionMode: "bypassPermissions", // LOCKED default (spec 03).
			RulesFile:      ".flanders/rules.md",
			StreamInput:    true,
		},
		Phases: Phases{
			Discuss: AgentClass{Model: "opus", Effort: "high"},
			Plan:    AgentClass{Model: "opus", Effort: "high"},
			Build:   AgentClass{Model: "opus", Effort: "high"},
			Test:    AgentClass{Model: "sonnet", Effort: "medium"},
		},
		Subagents: Subagents{
			Model:   "sonnet",
			Effort:  "low",
			Classes: map[string]AgentClass{},
		},
		Context: Context{
			WindowTokens: 200000,
			SoftPct:      0.75,
			HardPct:      0.90,
		},
		Guardrails: Guardrails{
			MaxIterations:    100,
			StallN:           3,
			IterationTimeout: Duration{20 * time.Minute},
		},
		Usage: Usage{
			OnLimit:   "wait",
			MaxCycles: 0,
			Backoff:   Duration{30 * time.Minute},
		},
		Git: Git{
			Enabled:       true,
			InitIfMissing: true,
			CommitEach:    "progress",
			MessageTmpl:   "Flanders: {phase} #{iter} — {task} [{result}]",
		},
	}
}

// Load reads and validates the config at path. It overlays the file on top of
// Default(), so any key the user omits keeps its documented default. A missing
// file returns an error (the caller decides whether to run `flanders init`); the
// underlying fs.ErrNotExist is preserved via %w for errors.Is checks.
//
// Load does NOT enforce the build-only test-command requirement — that belongs to
// ValidateForBuild, since the plan phase legitimately runs without a test command.
func Load(path string) (*Config, error) {
	cfg := Default()
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("load config %q: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %q: %w", path, err)
	}
	return &cfg, nil
}

// Validate checks enum-like fields and numeric ranges that have a single correct
// shape regardless of phase. It is called by Load. It deliberately does not check
// the test command (see ValidateForBuild) because that requirement is phase-scoped.
func (c *Config) Validate() error {
	if err := validateEffort("phases.discuss.effort", c.Phases.Discuss.Effort); err != nil {
		return err
	}
	if err := validateEffort("phases.plan.effort", c.Phases.Plan.Effort); err != nil {
		return err
	}
	if err := validateEffort("phases.build.effort", c.Phases.Build.Effort); err != nil {
		return err
	}
	if err := validateEffort("phases.test.effort", c.Phases.Test.Effort); err != nil {
		return err
	}
	if err := validateEffort("subagents.effort", c.Subagents.Effort); err != nil {
		return err
	}
	// Per-class subagent overrides need only be valid when they set an effort.
	for _, name := range sortedKeys(c.Subagents.Classes) {
		class := c.Subagents.Classes[name]
		if class.Effort != "" {
			if err := validateEffort("subagents."+name+".effort", class.Effort); err != nil {
				return err
			}
		}
	}

	if !oneOf(c.Agent.PermissionMode, validPermissionMode) {
		return fmt.Errorf("agent.permission_mode %q: must be one of %s",
			c.Agent.PermissionMode, strings.Join(validPermissionMode, ", "))
	}
	if !oneOf(c.Usage.OnLimit, validOnLimit) {
		return fmt.Errorf("usage.on_limit %q: must be one of %s",
			c.Usage.OnLimit, strings.Join(validOnLimit, ", "))
	}
	if !oneOf(c.Git.CommitEach, validCommitEach) {
		return fmt.Errorf("git.commit_each %q: must be one of %s",
			c.Git.CommitEach, strings.Join(validCommitEach, ", "))
	}

	if c.Context.WindowTokens < 0 {
		return fmt.Errorf("context.window_tokens %d: must be >= 0 (0 = auto-detect by model)", c.Context.WindowTokens)
	}
	if c.Context.SoftPct <= 0 || c.Context.SoftPct > 1 {
		return fmt.Errorf("context.soft_pct %v: must be in (0, 1]", c.Context.SoftPct)
	}
	if c.Context.HardPct <= 0 || c.Context.HardPct > 1 {
		return fmt.Errorf("context.hard_pct %v: must be in (0, 1]", c.Context.HardPct)
	}
	if c.Context.SoftPct > c.Context.HardPct {
		return fmt.Errorf("context.soft_pct %v must be <= context.hard_pct %v", c.Context.SoftPct, c.Context.HardPct)
	}

	if c.Guardrails.MaxIterations < 1 {
		return fmt.Errorf("guardrails.max_iterations %d: must be >= 1", c.Guardrails.MaxIterations)
	}
	if c.Guardrails.StallN < 1 {
		return fmt.Errorf("guardrails.stall_n %d: must be >= 1", c.Guardrails.StallN)
	}
	if c.Guardrails.IterationTimeout.Duration <= 0 {
		return fmt.Errorf("guardrails.iteration_timeout %v: must be > 0", c.Guardrails.IterationTimeout.Duration)
	}
	if c.Usage.MaxCycles < 0 {
		return fmt.Errorf("usage.max_cycles %d: must be >= 0 (0 = unlimited)", c.Usage.MaxCycles)
	}
	if c.Usage.Backoff.Duration <= 0 {
		return fmt.Errorf("usage.backoff %v: must be > 0", c.Usage.Backoff.Duration)
	}
	return nil
}

// ValidateForBuild enforces the build-phase precondition that a test command is
// configured — the harness's ground-truth done-gate (spec 00 decision 2, spec 03).
// Run this before entering a build loop; the plan phase does not need it.
func (c *Config) ValidateForBuild() error {
	if strings.TrimSpace(c.Commands.Test) == "" {
		return fmt.Errorf("commands.test is required for the build phase (it is the harness's done-gate)")
	}
	return nil
}

// PhaseClass returns the model+effort an agent should run with for a given phase.
// It is the single source of truth for phase→class resolution (spec 07): the four
// configured phases map to their [phases.*] table, and "split" deliberately reuses
// [phases.plan] (split is not its own class — spec 07 §Two layers). An unknown
// phase is an error rather than a silent default, so a typo surfaces at invocation
// time instead of silently running the wrong model.
//
// Subagent-class resolution (the [subagents] default + per-class override merge)
// is a separate concern — the harness only ever invokes phase (lead) agents;
// subagents are spawned by the lead inside its own session. See SubagentClass.
func (c *Config) PhaseClass(phase string) (AgentClass, error) {
	switch phase {
	case "discuss":
		return c.Phases.Discuss, nil
	case "plan":
		return c.Phases.Plan, nil
	case "build":
		return c.Phases.Build, nil
	case "test":
		return c.Phases.Test, nil
	case "split":
		return c.Phases.Plan, nil // split reuses plan (spec 07)
	default:
		return AgentClass{}, fmt.Errorf("unknown phase %q (want discuss|plan|build|test|split)", phase)
	}
}

// SubagentClass resolves the model+effort for a subagent class by name, the
// single source of truth for subagent-class resolution (spec 07). It is the
// companion to PhaseClass for the lighter agents a lead spins up inside its own
// session: it starts from the global [subagents] default (sonnet/low) and overlays
// any [subagents.<name>] override.
//
// Two deliberate differences from PhaseClass: (1) an unknown name is NOT an error
// — subagent class names are open-ended (a lead may spawn any-named helper), so a
// name with no [subagents.<name>] table simply resolves to the global default;
// (2) overrides are merged field-by-field, not wholesale — a table that sets only
// `model` (or only `effort`) keeps the global default for the field it omits, so a
// partial override never blanks a field to "" (mirrors agentClassFromMap, which
// leaves an unset field empty).
func (c *Config) SubagentClass(name string) AgentClass {
	class := AgentClass{Model: c.Subagents.Model, Effort: c.Subagents.Effort}
	if override, ok := c.Subagents.Classes[name]; ok {
		if override.Model != "" {
			class.Model = override.Model
		}
		if override.Effort != "" {
			class.Effort = override.Effort
		}
	}
	return class
}

func validateEffort(field, value string) error {
	if !oneOf(value, validEfforts) {
		return fmt.Errorf("%s %q: must be one of %s", field, value, strings.Join(validEfforts, ", "))
	}
	return nil
}

func oneOf(value string, allowed []string) bool {
	for _, a := range allowed {
		if value == a {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]AgentClass) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
