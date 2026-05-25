// Package journal is the harness's append-only, per-loop history under
// .flanders/journal/ (specs/01-ralph-loop.md §journal, specs/04-tui.md §journal).
//
// Why the journal exists: every loop is a *fresh* `claude` session that forgets
// everything when it exits (spec 01 §principle). The journal is therefore the
// only memory across those fresh contexts — it is what the TUI's history pane
// renders, and (with the task files + git) one of the ground-truth tiers
// state.json is rebuilt from (spec 09 §state hierarchy: journal is tier 2, the
// state cache is tier 3). It is append-only: an entry, once written, is never
// mutated, so the record of what each loop did stays trustworthy.
//
// Why two files per loop. The spec says each loop stores "the raw stream-json
// plus a short summary." Those are kept as two artifacts per entry:
//
//	<seq>.json         — the Summary (task, files, test result, cost, tokens, …)
//	<seq>.stream.jsonl — the verbatim NDJSON the CLI emitted (for drill-in)
//
// The split is load-bearing for the TUI: the history list renders Summaries,
// which are tiny, so listing N loops is N small JSON parses and never touches the
// (potentially megabytes-large) raw transcripts. Only when the operator drills
// into one loop is that loop's stream opened. A single combined file would force
// the list to read every transcript just to show a one-line history row.
//
// Why the stream is written before the summary. The summary file is the *commit
// marker* for an entry: List/Last key off summaries (`*.json`), not streams. So
// Append writes the stream first and the summary last — a crash between the two
// leaves an orphan stream with no summary, which List simply ignores, rather than
// a listed entry whose drill-in would 404. On the next Append the orphan's seq is
// reused (no summary claims it yet) and its stream is overwritten, so failed
// writes neither leak seq numbers nor accumulate junk.
//
// Why the journal owns the sequence number. The seq is allocated here by scanning
// existing summary filenames, not supplied by the caller. That keeps the journal
// a self-contained append-only log whose ordering cannot be corrupted by a caller
// passing a stale or duplicate index — exactly the independence the tier
// hierarchy needs, since state.json (which tracks iteration counts) is only a
// derived cache and must never be the thing the history depends on.
//
// Why this package does not import the stream-json parser (Phase 2.1). The
// journal is a *persistence* concern; parsing wire shapes is a separate one. The
// caller (the loop driver, Phase 3) parses the stream into a LoopObservation and
// hands the journal the derived primitive fields plus the raw bytes to archive.
// Keeping that boundary means the on-disk record format has exactly one owner
// (here) and survives changes to the wire protocol.
package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"flanders/src/lib/task"
)

// SchemaVersion is the on-disk generation of a Summary. It is stamped into every
// entry so a future format change can be detected. Unlike state.json (a cache we
// rebuild on a version mismatch), the journal is history we want to keep reading,
// so reads tolerate an unfamiliar version rather than discarding the entry.
const SchemaVersion = 1

const (
	summaryExt = ".json"        // <seq>.json — the per-loop Summary
	streamExt  = ".stream.jsonl" // <seq>.stream.jsonl — the raw NDJSON transcript
	seqWidth   = 6               // zero-pad width for filenames (cosmetic; ordering parses the int)
)

// Tokens mirrors the token-usage fields the stream-json carries (spec 08 §live
// token tracking / result event). Stored per loop so the TUI history and the
// throughput accounting read a loop's totals without re-parsing the transcript.
type Tokens struct {
	Input         int `json:"input"`
	Output        int `json:"output"`
	CacheRead     int `json:"cache_read"`
	CacheCreation int `json:"cache_creation"`
}

// Total is the context-occupancy sum spec 08 uses for the context-pressure
// estimate: input + cache_read + cache_creation + output. Summing all four
// matches the spec's "err on the side of over-counting so trips fire early."
func (t Tokens) Total() int { return t.Input + t.Output + t.CacheRead + t.CacheCreation }

// TestResult is the harness-owned ground-truth verdict for a loop (spec 01
// §done-detection: the canonical test command's exit code is truth, the agent's
// self-report is advisory). Ran distinguishes "tests were not run this loop"
// (a plan loop, or a loop the harness killed before the verify step) from
// "ran and exited non-zero" — both leave ExitCode 0 otherwise indistinguishable.
type TestResult struct {
	Command  string `json:"command"`
	Ran      bool   `json:"ran"`
	ExitCode int    `json:"exit_code"`
}

// Passed reports the ground-truth pass: the test command ran and exited 0.
func (r TestResult) Passed() bool { return r.Ran && r.ExitCode == 0 }

// Subagent records a subagent the loop spawned, detected from a Task/Agent
// tool_use in the stream (spec 08 §content blocks, spec 07 §visibility). Captured
// so a journal drill-in (and any agent-tree replay) knows which models did the
// work. Model/effort are optional — present only when the spawn input named them.
type Subagent struct {
	Name   string `json:"name"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// Summary is the short, cheap-to-read record of one loop (spec 01 §journal). The
// TUI history list renders these without touching the raw stream; drill-in then
// loads <seq>.stream.jsonl on demand. Every field the list/meters need lives
// here so reading history is one small JSON parse per loop.
//
// Seq is assigned by Append (the caller's value is ignored) and is authoritative
// from the filename on read, so list ordering always matches allocation order
// even if a stored field were ever stale.
type Summary struct {
	SchemaVersion int    `json:"schema_version"`
	Seq           int    `json:"seq"`
	Phase         string `json:"phase"`          // plan | build | orchestrate | discuss
	Task          string `json:"task,omitempty"` // task id targeted (empty for plan/discuss)
	SessionID     string `json:"session_id"`     // claude session; state.last_session_id cross-ref (spec 09)

	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at"`
	DurationMS int64     `json:"duration_ms"` // from result.duration_ms when present; harness wall-clock otherwise

	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`

	Files     []string   `json:"files,omitempty"` // files touched, inferred by the harness from git diff (spec 02)
	Subagents []Subagent `json:"subagents,omitempty"`

	Test   TestResult `json:"test"`
	Cost   float64    `json:"cost_usd"` // total_cost_usd — info/throughput only, never a stop (spec 00)
	Tokens Tokens     `json:"tokens"`

	StatusBefore task.Status `json:"status_before,omitempty"`
	StatusAfter  task.Status `json:"status_after,omitempty"`
	Reason       task.Reason `json:"reason,omitempty"` // set iff the loop left the task blocked (spec 02 taxonomy)

	Error string `json:"error,omitempty"` // result error / classification; empty on a clean loop
}

// CorruptError marks a single journal entry whose summary exists but cannot be
// parsed. Read returns it so a caller can distinguish a damaged entry from a
// missing one; List, which favors resilience for the TUI, skips such entries
// instead (see List).
type CorruptError struct {
	Path string
	Err  error
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("journal: %q is corrupt: %v", e.Path, e.Err)
}

func (e *CorruptError) Unwrap() error { return e.Err }

// Journal is an append-only history bound to a directory (typically
// paths.Journal, i.e. .flanders/journal/). All methods are safe to call on a
// directory that does not exist yet only after Open, which creates it.
type Journal struct {
	dir string
}

// Open binds a Journal to dir, creating it (and parents) if absent. It is
// idempotent — safe to call on every startup, mirroring paths.EnsureFlanders.
func Open(dir string) (*Journal, error) {
	if dir == "" {
		return nil, errors.New("journal: Open needs a directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("journal: create %q: %w", dir, err)
	}
	return &Journal{dir: dir}, nil
}

// Dir returns the journal's directory.
func (j *Journal) Dir() string { return j.dir }

// Append writes one loop's record and returns the seq it was assigned. The seq
// is allocated here (max existing + 1), so the caller's Summary.Seq is ignored
// and overwritten. raw is the verbatim stream-json transcript to archive; a nil
// raw writes an empty stream file so the invariant "every listed entry has a
// readable stream" always holds. The stream is written before the summary so the
// summary acts as the entry's commit marker (see package doc).
func (j *Journal) Append(s *Summary, raw io.Reader) (int, error) {
	if s == nil {
		return 0, errors.New("journal: Append needs a summary")
	}
	seq, err := j.nextSeq()
	if err != nil {
		return 0, err
	}
	s.Seq = seq
	if s.SchemaVersion == 0 {
		s.SchemaVersion = SchemaVersion
	}

	// Stream first: it is the non-marker half, so an interrupted Append leaves at
	// most an orphan stream that List ignores and the next Append overwrites.
	if raw == nil {
		raw = bytes.NewReader(nil)
	}
	if err := atomicWriteFrom(filepath.Join(j.dir, streamName(seq)), raw); err != nil {
		return 0, err
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("journal: marshal summary: %w", err)
	}
	data = append(data, '\n')
	if err := atomicWriteFrom(filepath.Join(j.dir, summaryName(seq)), bytes.NewReader(data)); err != nil {
		return 0, err
	}
	return seq, nil
}

// List returns every entry's Summary, ordered by seq ascending (i.e. loop order).
// It favors resilience over strictness — the TUI history pane must render even if
// one entry is damaged — so a summary file that cannot be read or parsed is
// skipped rather than failing the whole call (mirroring the stream parser's
// skip-unparseable-lines rule, spec 08). Only a directory-level failure is
// returned as an error.
func (j *Journal) List() ([]*Summary, error) {
	matches, err := filepath.Glob(filepath.Join(j.dir, "*"+summaryExt))
	if err != nil {
		return nil, fmt.Errorf("journal: glob %q: %w", j.dir, err)
	}
	out := make([]*Summary, 0, len(matches))
	for _, path := range matches {
		seq, ok := parseSeq(filepath.Base(path))
		if !ok {
			continue // not one of our <seq>.json files
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue // unreadable entry — skip, keep the rest renderable
		}
		var s Summary
		if json.Unmarshal(data, &s) != nil {
			continue // corrupt entry — skip
		}
		s.Seq = seq // filename is authoritative for ordering
		out = append(out, &s)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Seq < out[b].Seq })
	return out, nil
}

// Read returns the Summary for a single entry. A missing entry returns an error
// wrapping os.ErrNotExist; a present-but-unparseable one returns a *CorruptError.
func (j *Journal) Read(seq int) (*Summary, error) {
	path := filepath.Join(j.dir, summaryName(seq))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err // includes os.ErrNotExist
	}
	var s Summary
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, &CorruptError{Path: path, Err: err}
	}
	s.Seq = seq
	return &s, nil
}

// ReadStream opens the raw stream-json transcript for an entry for the TUI
// drill-in. The caller must Close the returned reader. A missing stream returns
// an error wrapping os.ErrNotExist.
func (j *Journal) ReadStream(seq int) (io.ReadCloser, error) {
	f, err := os.Open(filepath.Join(j.dir, streamName(seq)))
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Last returns the most recent entry's Summary, or (nil, nil) when the journal is
// empty. It is the seam state.Rebuild needs to recover last_session_id and the
// resume cursor from tier-2 history (spec 09; the hook noted in state.Rebuild).
func (j *Journal) Last() (*Summary, error) {
	seqs, err := j.seqs()
	if err != nil {
		return nil, err
	}
	if len(seqs) == 0 {
		return nil, nil
	}
	return j.Read(seqs[len(seqs)-1])
}

// Len reports how many entries the journal holds. The orchestrator uses it (with
// per-phase tallies from List) to seed state.Iter on a rebuild — the journal-tier
// enrichment that state.Rebuild deliberately left to this package (spec 09).
func (j *Journal) Len() (int, error) {
	seqs, err := j.seqs()
	if err != nil {
		return 0, err
	}
	return len(seqs), nil
}

// nextSeq allocates the next append-order index: one past the highest existing
// summary. Allocation reads only filenames (cheap, no parse) and the filename is
// the single source of truth for an entry's seq.
func (j *Journal) nextSeq() (int, error) {
	seqs, err := j.seqs()
	if err != nil {
		return 0, err
	}
	if len(seqs) == 0 {
		return 1, nil
	}
	return seqs[len(seqs)-1] + 1, nil
}

// seqs returns the sorted seq numbers of all summary files in the directory.
func (j *Journal) seqs() ([]int, error) {
	matches, err := filepath.Glob(filepath.Join(j.dir, "*"+summaryExt))
	if err != nil {
		return nil, fmt.Errorf("journal: glob %q: %w", j.dir, err)
	}
	out := make([]int, 0, len(matches))
	for _, path := range matches {
		if n, ok := parseSeq(filepath.Base(path)); ok {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out, nil
}

func summaryName(seq int) string { return fmt.Sprintf("%0*d%s", seqWidth, seq, summaryExt) }
func streamName(seq int) string  { return fmt.Sprintf("%0*d%s", seqWidth, seq, streamExt) }

// parseSeq extracts the seq from a summary filename (<digits>.json), returning
// ok=false for anything that is not one of our entries — notably the
// <seq>.stream.jsonl files, which end in .jsonl and so never match summaryExt.
func parseSeq(name string) (int, bool) {
	stem, ok := strings.CutSuffix(name, summaryExt)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(stem)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// atomicWriteFrom streams r into path via a temp file in the same directory +
// rename, the same atomicity discipline as state.Save/task.WriteFile: a crash
// mid-write can never leave a torn file a reader would choke on. The temp lives
// in the destination directory so the rename stays on one filesystem.
func atomicWriteFrom(path string, r io.Reader) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("journal: create %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".journal-*.tmp")
	if err != nil {
		return fmt.Errorf("journal: create temp in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return fmt.Errorf("journal: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("journal: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("journal: rename temp to %q: %w", path, err)
	}
	return nil
}
