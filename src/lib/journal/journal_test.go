package journal

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flanders/src/lib/task"
)

// sampleStream is a tiny but representative NDJSON transcript — the kind of raw
// stream-json a loop archives. The journal stores it verbatim, so the exact
// shapes here don't matter; only that bytes round-trip unchanged.
const sampleStream = `{"type":"system","subtype":"init","session_id":"sess-123","model":"opus"}
{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}
{"type":"result","subtype":"success","total_cost_usd":0.12,"duration_ms":4200}
`

func newSummary() *Summary {
	start := time.Date(2026, 5, 25, 15, 0, 0, 0, time.UTC)
	return &Summary{
		Phase:      "build",
		Task:       "0007",
		SessionID:  "sess-123",
		StartedAt:  start,
		EndedAt:    start.Add(4 * time.Second),
		DurationMS: 4200,
		Model:      "opus",
		Effort:     "high",
		Files:      []string{"src/lib/journal/journal.go", "src/lib/journal/journal_test.go"},
		Subagents:  []Subagent{{Name: "Explore", Model: "sonnet", Effort: "medium"}},
		Test:       TestResult{Command: "go test ./...", Ran: true, ExitCode: 0},
		Cost:       0.12,
		Tokens:     Tokens{Input: 1000, Output: 200, CacheRead: 50, CacheCreation: 10},
		StatusBefore: task.StatusActive,
		StatusAfter:  task.StatusDone,
	}
}

// TestAppendRoundTrip is the acceptance criterion for task 1.6: a loop produces a
// re-readable journal entry. It writes a full summary + raw stream and asserts
// both come back byte-for-byte / field-for-field.
func TestAppendRoundTrip(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	want := newSummary()
	seq, err := j.Append(want, strings.NewReader(sampleStream))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if seq != 1 {
		t.Fatalf("first seq = %d, want 1", seq)
	}

	got, err := j.Read(seq)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, SchemaVersion)
	}
	if got.Seq != 1 || got.Phase != "build" || got.Task != "0007" || got.SessionID != "sess-123" {
		t.Errorf("identity fields lost: %+v", got)
	}
	if !got.StartedAt.Equal(want.StartedAt) || !got.EndedAt.Equal(want.EndedAt) {
		t.Errorf("timestamps lost: started=%v ended=%v", got.StartedAt, got.EndedAt)
	}
	if got.DurationMS != want.DurationMS || got.Cost != want.Cost {
		t.Errorf("duration/cost lost: dur=%d cost=%v", got.DurationMS, got.Cost)
	}
	if got.Tokens != want.Tokens {
		t.Errorf("tokens lost: got %+v want %+v", got.Tokens, want.Tokens)
	}
	if got.Test != want.Test {
		t.Errorf("test result lost: got %+v want %+v", got.Test, want.Test)
	}
	if len(got.Files) != 2 || got.Files[0] != want.Files[0] {
		t.Errorf("files lost: %v", got.Files)
	}
	if len(got.Subagents) != 1 || got.Subagents[0] != want.Subagents[0] {
		t.Errorf("subagents lost: %v", got.Subagents)
	}
	if got.StatusBefore != task.StatusActive || got.StatusAfter != task.StatusDone {
		t.Errorf("status transition lost: before=%q after=%q", got.StatusBefore, got.StatusAfter)
	}

	// The raw stream must come back verbatim for the TUI drill-in.
	rc, err := j.ReadStream(seq)
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if string(raw) != sampleStream {
		t.Errorf("raw stream not verbatim:\n got %q\nwant %q", raw, sampleStream)
	}
}

// TestAppendAssignsIncreasingSeq verifies the journal owns the append-order index
// and ignores any caller-supplied Seq.
func TestAppendAssignsIncreasingSeq(t *testing.T) {
	j, _ := Open(t.TempDir())
	for want := 1; want <= 3; want++ {
		s := newSummary()
		s.Seq = 99 // should be ignored/overwritten
		got, err := j.Append(s, strings.NewReader("x"))
		if err != nil {
			t.Fatalf("Append #%d: %v", want, err)
		}
		if got != want {
			t.Fatalf("Append #%d returned seq %d, want %d", want, got, want)
		}
		if s.Seq != want {
			t.Errorf("Append did not overwrite caller Seq: got %d, want %d", s.Seq, want)
		}
	}
}

// TestSeqResumesAcrossOpen confirms a fresh Open of a populated directory
// continues numbering — the journal is append-only across process restarts.
func TestSeqResumesAcrossOpen(t *testing.T) {
	dir := t.TempDir()
	j1, _ := Open(dir)
	if _, err := j1.Append(newSummary(), strings.NewReader("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := j1.Append(newSummary(), strings.NewReader("b")); err != nil {
		t.Fatal(err)
	}

	j2, _ := Open(dir) // simulate a restart
	seq, err := j2.Append(newSummary(), strings.NewReader("c"))
	if err != nil {
		t.Fatal(err)
	}
	if seq != 3 {
		t.Fatalf("resumed seq = %d, want 3", seq)
	}
}

func TestListOrderedAndLast(t *testing.T) {
	j, _ := Open(t.TempDir())
	if list, err := j.List(); err != nil || len(list) != 0 {
		t.Fatalf("empty List = %v, %v; want empty,nil", list, err)
	}
	if last, err := j.Last(); err != nil || last != nil {
		t.Fatalf("empty Last = %v, %v; want nil,nil", last, err)
	}

	tasks := []string{"0001", "0002", "0003"}
	for _, id := range tasks {
		s := newSummary()
		s.Task = id
		if _, err := j.Append(s, strings.NewReader(id)); err != nil {
			t.Fatal(err)
		}
	}

	list, err := j.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("List len = %d, want 3", len(list))
	}
	for i, s := range list {
		if s.Seq != i+1 {
			t.Errorf("List[%d].Seq = %d, want %d (must be seq-ordered)", i, s.Seq, i+1)
		}
		if s.Task != tasks[i] {
			t.Errorf("List[%d].Task = %q, want %q", i, s.Task, tasks[i])
		}
	}

	last, err := j.Last()
	if err != nil {
		t.Fatalf("Last: %v", err)
	}
	if last == nil || last.Seq != 3 || last.Task != "0003" {
		t.Errorf("Last = %+v, want seq 3 task 0003", last)
	}

	if n, err := j.Len(); err != nil || n != 3 {
		t.Errorf("Len = %d, %v; want 3,nil", n, err)
	}
}

// TestListSkipsCorruptEntry asserts the TUI-facing resilience contract: a damaged
// summary is skipped by List (so history still renders) while Read on that same
// entry surfaces a *CorruptError.
func TestListSkipsCorruptEntry(t *testing.T) {
	dir := t.TempDir()
	j, _ := Open(dir)
	if _, err := j.Append(newSummary(), strings.NewReader("ok")); err != nil {
		t.Fatal(err)
	}

	// Hand-write a corrupt second entry directly (bypassing Append).
	corrupt := filepath.Join(dir, summaryName(2))
	if err := os.WriteFile(corrupt, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := j.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Seq != 1 {
		t.Fatalf("List = %d entries %v; want only the healthy seq 1", len(list), list)
	}

	_, err = j.Read(2)
	var ce *CorruptError
	if !errors.As(err, &ce) {
		t.Fatalf("Read(corrupt) error = %v; want *CorruptError", err)
	}
}

func TestMissingEntry(t *testing.T) {
	j, _ := Open(t.TempDir())
	if _, err := j.Read(42); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Read(missing) = %v; want os.ErrNotExist", err)
	}
	if _, err := j.ReadStream(42); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadStream(missing) = %v; want os.ErrNotExist", err)
	}
}

// TestNilRawWritesReadableStream verifies the invariant that every listed entry
// has a readable stream, even when the loop archived no raw bytes.
func TestNilRawWritesReadableStream(t *testing.T) {
	j, _ := Open(t.TempDir())
	seq, err := j.Append(newSummary(), nil)
	if err != nil {
		t.Fatalf("Append(nil raw): %v", err)
	}
	rc, err := j.ReadStream(seq)
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if len(b) != 0 {
		t.Errorf("nil raw produced %d bytes, want empty", len(b))
	}
}

// TestStreamFilesNotListedAsSummaries guards the filename convention: the
// <seq>.stream.jsonl transcripts must never be mistaken for <seq>.json summaries.
func TestStreamFilesNotListedAsSummaries(t *testing.T) {
	dir := t.TempDir()
	j, _ := Open(dir)
	if _, err := j.Append(newSummary(), strings.NewReader(sampleStream)); err != nil {
		t.Fatal(err)
	}
	// Exactly one summary and one stream file should exist for one entry.
	ents, _ := os.ReadDir(dir)
	var summaries, streams int
	for _, e := range ents {
		switch {
		case strings.HasSuffix(e.Name(), streamExt):
			streams++
		case strings.HasSuffix(e.Name(), summaryExt):
			summaries++
		}
	}
	if summaries != 1 || streams != 1 {
		t.Fatalf("file layout: %d summaries, %d streams; want 1 and 1", summaries, streams)
	}
	if list, _ := j.List(); len(list) != 1 {
		t.Errorf("List counted %d entries; stream file leaked in", len(list))
	}
}

func TestHelpers(t *testing.T) {
	if got := (Tokens{Input: 100, Output: 20, CacheRead: 5, CacheCreation: 3}).Total(); got != 128 {
		t.Errorf("Tokens.Total = %d, want 128", got)
	}
	if !(TestResult{Ran: true, ExitCode: 0}).Passed() {
		t.Error("ran+exit0 should be Passed")
	}
	if (TestResult{Ran: true, ExitCode: 1}).Passed() {
		t.Error("ran+exit1 should not be Passed")
	}
	if (TestResult{Ran: false, ExitCode: 0}).Passed() {
		t.Error("not-run should not be Passed (even with exit 0 zero value)")
	}
}

func TestOpenRejectsEmptyDir(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Error("Open(\"\") should error")
	}
}
