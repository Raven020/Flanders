// Package task is the on-disk model for a single unit of work: one file under
// specs/tasks/, made of YAML frontmatter + a markdown body
// (specs/02-plan-and-tasks.md).
//
// Why this package is the spine of the harness: the plan loop *writes* these
// files, the build loop *reads* them, and the selector, state cache, and journal
// all key off a task's id/status. So this is the single source of truth for what
// a task is — every consumer imports it; none re-parses task files itself.
//
// Why a yaml.Node backs the frontmatter (and not a plain struct): a task file is
// edited by three parties — the human, the agent, and the harness — and the spec
// requires it to round-trip without loss. A struct with fixed fields would drop
// unknown keys (the spec leaves `files`/`notes`/`attempts` OPEN and expects more
// to come), reorder keys, and strip the inline comments the example file carries.
// Decoding into a yaml.Node and re-emitting it preserves all three. The node is
// therefore the authority; the typed accessors/setters below are a thin, typed
// view over it, so there is exactly one copy of the data and no struct↔node drift.
//
// Why mutation goes through setters only: the agent's sole structured edit is the
// `status:` field, and the harness writes status/reason directly when it ends a
// loop itself (spec 02 §mutation ownership). SetStatus/SetBlocked keep the
// "reason iff blocked" invariant intact automatically, so an invalid combination
// is hard to reach by construction rather than only caught at Validate time.
package task

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Status is a task's lifecycle state. The four values are locked by spec 02.
type Status string

const (
	StatusPending Status = "pending" // not started; eligible once deps are done
	StatusActive  Status = "active"  // a loop is working it right now
	StatusDone    Status = "done"    // complete (agent-flipped or harness-inferred)
	StatusBlocked Status = "blocked" // cannot proceed; see Reason
)

// Reason explains a blocked task. Required exactly when Status is blocked, and
// drawn from the locked taxonomy below (spec 02). The taxonomy is meaningful: it
// tells the orchestrator how to resolve the block — a fresh split, a batched
// re-plan, waiting, or escalation.
type Reason string

const (
	ReasonContextOverreach Reason = "context-overreach" // too big for one pass → fresh split (spec 06)
	ReasonNewScope         Reason = "new-scope"         // missing requirement → batched re-plan
	ReasonDependency       Reason = "dependency"        // waiting on another task / external thing
	ReasonError            Reason = "error"             // repeated failure → escalate (see attempts)
)

// Frontmatter keys. Centralized so reads, writes, and New all agree on spelling.
const (
	keyID         = "id"
	keyStatus     = "status"
	keyReason     = "reason"
	keyDeps       = "deps"
	keyAcceptance = "acceptance"
	keyNotes      = "notes"
	keyFiles      = "files"
	keyAttempts   = "attempts"
)

var (
	// ErrNoFrontmatter is returned when a file does not open with a `---` line.
	ErrNoFrontmatter = errors.New("task: missing YAML frontmatter (file must start with '---')")
	// ErrUnterminated is returned when the opening `---` has no matching close.
	ErrUnterminated = errors.New("task: unterminated YAML frontmatter (no closing '---')")
)

// validStatuses / validReasons back the enum checks in Validate. Maps (not
// slices) so the check is a single lookup and the sets are self-documenting.
var validStatuses = map[Status]bool{
	StatusPending: true, StatusActive: true, StatusDone: true, StatusBlocked: true,
}

var validReasons = map[Reason]bool{
	ReasonContextOverreach: true, ReasonNewScope: true, ReasonDependency: true, ReasonError: true,
}

// Task is one task file. front is the frontmatter mapping node (the authority);
// Body is the markdown after the closing `---` (preserved verbatim); Path is the
// source file, when read from disk (never serialized into the file itself).
type Task struct {
	Path string

	front *yaml.Node
	Body  string
}

// New builds a fresh task with the canonical key order (id, status, deps,
// acceptance) the plan loop emits. deps may be nil (renders as `[]`). id and deps
// are written as numeric scalars to match the spec's example formatting.
func New(id string, status Status, deps []string, acceptance, body string) *Task {
	t := &Task{front: &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, Body: body}
	t.setNumScalar(keyID, id)
	t.setStrScalar(keyStatus, string(status))
	t.SetDeps(deps)
	t.setStrScalar(keyAcceptance, acceptance)
	return t
}

// Parse reads a task from its raw bytes: split frontmatter from body, decode the
// frontmatter into a node we keep, and retain the body untouched.
func Parse(data []byte) (*Task, error) {
	fm, body, err := splitFrontmatter(data)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(fm), &doc); err != nil {
		return nil, fmt.Errorf("task: parse frontmatter: %w", err)
	}
	front, err := mappingFromDoc(&doc)
	if err != nil {
		return nil, err
	}
	return &Task{front: front, Body: body}, nil
}

// ParseFile reads and parses the task file at path, recording Path.
func ParseFile(path string) (*Task, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("task: read %q: %w", path, err)
	}
	t, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("task: %q: %w", path, err)
	}
	t.Path = path
	return t, nil
}

// Bytes serializes the task back to the on-disk form: `---` + frontmatter + `---`
// + body. The frontmatter is re-emitted from the node, so unknown keys, key
// order, and inline comments survive a parse→serialize round-trip.
func (t *Task) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("---\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(t.front); err != nil {
		return nil, fmt.Errorf("task: encode frontmatter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("task: close encoder: %w", err)
	}
	buf.WriteString("---\n")
	buf.WriteString(t.Body)
	return buf.Bytes(), nil
}

// WriteFile serializes the task and writes it atomically (temp file in the same
// directory + rename), so a crash or a concurrent reader never sees a
// half-written task file. If path is empty it falls back to t.Path.
func (t *Task) WriteFile(path string) error {
	if path == "" {
		path = t.Path
	}
	if path == "" {
		return errors.New("task: WriteFile needs a path")
	}
	data, err := t.Bytes()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".task-*.tmp")
	if err != nil {
		return fmt.Errorf("task: create temp in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("task: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("task: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("task: rename temp to %q: %w", path, err)
	}
	t.Path = path
	return nil
}

// --- Typed reads (all read straight from the node) ---

// ID returns the `id` frontmatter value verbatim (leading zeros preserved).
func (t *Task) ID() string { return t.scalar(keyID) }

// Status returns the `status` value (empty if unset).
func (t *Task) Status() Status { return Status(t.scalar(keyStatus)) }

// Reason returns the `reason` value (empty if unset).
func (t *Task) Reason() Reason { return Reason(t.scalar(keyReason)) }

// Acceptance returns the `acceptance` criterion.
func (t *Task) Acceptance() string { return t.scalar(keyAcceptance) }

// Notes returns the optional `notes` field.
func (t *Task) Notes() string { return t.scalar(keyNotes) }

// Deps returns the `deps` ids verbatim (preserving any zero-padding), or nil.
func (t *Task) Deps() []string { return t.seq(keyDeps) }

// Files returns the optional `files` hint list, or nil.
func (t *Task) Files() []string { return t.seq(keyFiles) }

// --- Typed writes (maintain invariants; mutate the node in place) ---

// SetStatus sets the status. Because reason is required iff blocked, moving to
// any non-blocked status clears a stale reason — so the invariant holds without
// the caller having to remember.
func (t *Task) SetStatus(s Status) {
	t.setStrScalar(keyStatus, string(s))
	if s != StatusBlocked {
		t.removeKey(keyReason)
	}
}

// SetBlocked sets status=blocked and records the reason in one step, which is the
// only valid way to reach a blocked state.
func (t *Task) SetBlocked(r Reason) {
	t.setStrScalar(keyStatus, string(StatusBlocked))
	t.setStrScalar(keyReason, string(r))
}

// SetNotes sets the optional `notes` field — the free-form handoff text the harness
// writes when it ends a loop itself (the context-pressure backstop writes a partial-
// progress summary here, spec 01 §context-pressure) and the plan/split passes read.
// An empty/whitespace-only value removes the key rather than leaving an empty scalar,
// so "no notes" round-trips as absence (the spec leaves `notes` optional, spec 02).
func (t *Task) SetNotes(notes string) {
	if strings.TrimSpace(notes) == "" {
		t.removeKey(keyNotes)
		return
	}
	t.setStrScalar(keyNotes, notes)
}

// SetDeps replaces the deps list (flow-style numeric scalars, like the spec
// example). Empty/nil renders as `[]`.
func (t *Task) SetDeps(deps []string) {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	for _, d := range deps {
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: d})
	}
	t.setNode(keyDeps, seq)
}

// Validate enforces the locked frontmatter rules (spec 02): id and acceptance
// present, status a known value, and reason present-and-valid exactly when the
// task is blocked. It does not enforce the OPEN optional fields.
func (t *Task) Validate() error {
	if strings.TrimSpace(t.ID()) == "" {
		return errors.New("task: missing required field 'id'")
	}
	status := t.Status()
	if !validStatuses[status] {
		return fmt.Errorf("task %s: invalid status %q (want pending|active|done|blocked)", t.ID(), status)
	}
	if strings.TrimSpace(t.Acceptance()) == "" {
		return fmt.Errorf("task %s: missing required field 'acceptance'", t.ID())
	}
	reason := t.Reason()
	if status == StatusBlocked {
		if reason == "" {
			return fmt.Errorf("task %s: status 'blocked' requires a 'reason'", t.ID())
		}
		if !validReasons[reason] {
			return fmt.Errorf("task %s: invalid reason %q (want context-overreach|new-scope|dependency|error)", t.ID(), reason)
		}
	} else if reason != "" {
		return fmt.Errorf("task %s: 'reason' is only valid when status is 'blocked' (status is %q)", t.ID(), status)
	}
	return nil
}

// --- node helpers (the only code that touches the mapping structure) ---

// pair returns the value node for key (and its index), or (-1, nil) if absent.
// A mapping node stores content as a flat [k0,v0,k1,v1,…] slice.
func (t *Task) pair(key string) (int, *yaml.Node) {
	c := t.front.Content
	for i := 0; i+1 < len(c); i += 2 {
		if c[i].Value == key {
			return i, c[i+1]
		}
	}
	return -1, nil
}

// scalar returns the string value of a scalar-valued key, or "".
func (t *Task) scalar(key string) string {
	if _, v := t.pair(key); v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}
	return ""
}

// seq returns the verbatim scalar values of a sequence-valued key, or nil.
func (t *Task) seq(key string) []string {
	_, v := t.pair(key)
	if v == nil || v.Kind != yaml.SequenceNode {
		return nil
	}
	out := make([]string, 0, len(v.Content))
	for _, e := range v.Content {
		out = append(out, e.Value)
	}
	return out
}

// setNode sets key to value, replacing an existing value or appending a new pair
// (preserving existing key order on update).
func (t *Task) setNode(key string, value *yaml.Node) {
	if i, _ := t.pair(key); i >= 0 {
		t.front.Content[i+1] = value
		return
	}
	t.front.Content = append(t.front.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func (t *Task) setStrScalar(key, value string) {
	t.setNode(key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}

func (t *Task) setNumScalar(key, value string) {
	t.setNode(key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: value})
}

// removeKey deletes a key/value pair if present.
func (t *Task) removeKey(key string) {
	if i, _ := t.pair(key); i >= 0 {
		t.front.Content = append(t.front.Content[:i], t.front.Content[i+2:]...)
	}
}

// mappingFromDoc extracts the root mapping from a decoded document. Empty
// frontmatter yields a fresh empty mapping (a valid, if incomplete, task);
// non-empty frontmatter that isn't a mapping is an error (a task must be keyed).
func mappingFromDoc(doc *yaml.Node) (*yaml.Node, error) {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root := doc.Content[0]
		if root.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("task: frontmatter must be a YAML mapping, got %s", kindName(root.Kind))
		}
		return root, nil
	}
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.SequenceNode:
		return "sequence"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	default:
		return "unknown"
	}
}

// splitFrontmatter separates the leading `---`-delimited YAML block from the
// markdown body. The closing delimiter is the first `---` line after the opening
// one, so a `---` horizontal rule inside the body is not mistaken for it. Both
// LF and CRLF line endings are handled, and a leading UTF-8 BOM is tolerated.
func splitFrontmatter(data []byte) (front, body string, err error) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	lines := splitKeepEOL(text)
	if len(lines) == 0 || !isDelimLine(lines[0]) {
		return "", "", ErrNoFrontmatter
	}
	var fm strings.Builder
	closeIdx := -1
	for i := 1; i < len(lines); i++ {
		if isDelimLine(lines[i]) {
			closeIdx = i
			break
		}
		fm.WriteString(lines[i])
	}
	if closeIdx == -1 {
		return "", "", ErrUnterminated
	}
	return fm.String(), strings.Join(lines[closeIdx+1:], ""), nil
}

// isDelimLine reports whether a line (with or without its EOL) is a `---` fence.
func isDelimLine(line string) bool {
	return strings.TrimSpace(line) == "---"
}

// splitKeepEOL splits s into lines, each retaining its trailing newline, so the
// pieces concatenate back to s exactly (lossless body reconstruction).
func splitKeepEOL(s string) []string {
	var out []string
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}
