package reconcile

import (
	"path/filepath"
	"testing"

	"flanders/src/lib/task"
)

// mkTaskFile writes a task at <dir>/<id>.md and returns the reloaded *Task (with
// Path set, so Reconcile's WriteFile("") targets the right file).
func mkTaskFile(t *testing.T, dir, id string, status task.Status) *task.Task {
	t.Helper()
	tk := task.New(id, status, nil, "tests pass for "+id, "## "+id+"\n")
	path := filepath.Join(dir, id+".md")
	if err := tk.WriteFile(path); err != nil {
		t.Fatalf("write task: %v", err)
	}
	reloaded, err := task.ParseFile(path)
	if err != nil {
		t.Fatalf("reload task: %v", err)
	}
	return reloaded
}

// onDisk reads the task's status back from disk to prove (or disprove) a write.
func onDisk(t *testing.T, tk *task.Task) task.Status {
	t.Helper()
	reloaded, err := task.ParseFile(tk.Path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return reloaded.Status()
}

// TestRespectsAgentDone: the agent flipped to done — the harness keeps it verbatim
// and does not rewrite the file (precedence: agent first).
func TestRespectsAgentDone(t *testing.T) {
	tk := mkTaskFile(t, t.TempDir(), "0001", task.StatusDone)
	res, err := Reconcile(tk, Signals{TestRan: true, TestPassed: true})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Action != ActionRespected || res.To != task.StatusDone || res.Wrote {
		t.Errorf("res = %+v, want Respected done, Wrote=false", res)
	}
}

// TestRespectsAgentBlocked: a blocked status (with its reason) is honored — the
// harness never second-guesses an explicit block, and the reason rides along.
func TestRespectsAgentBlocked(t *testing.T) {
	tk := task.New("0001", task.StatusPending, nil, "acc", "body\n")
	tk.SetBlocked(task.ReasonNewScope)
	path := filepath.Join(t.TempDir(), "0001.md")
	if err := tk.WriteFile(path); err != nil {
		t.Fatalf("write: %v", err)
	}
	reloaded, _ := task.ParseFile(path)

	res, err := Reconcile(reloaded, Signals{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Action != ActionRespected || res.To != task.StatusBlocked || res.Reason != task.ReasonNewScope {
		t.Errorf("res = %+v, want Respected blocked/new-scope", res)
	}
}

// TestPromotesOnPassingGate is the headline inference fallback: the agent left the
// task pending but the test gate passed, so the harness records the done it forgot
// to flip — and the write lands on disk.
func TestPromotesOnPassingGate(t *testing.T) {
	tk := mkTaskFile(t, t.TempDir(), "0001", task.StatusPending)
	res, err := Reconcile(tk, Signals{TestRan: true, TestPassed: true})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Action != ActionPromoted || res.To != task.StatusDone || !res.Wrote {
		t.Errorf("res = %+v, want Promoted done, Wrote=true", res)
	}
	if got := onDisk(t, tk); got != task.StatusDone {
		t.Errorf("on-disk status = %q, want done (promotion must persist)", got)
	}
}

// TestNoPromoteOnFailingGate: a pending task with a FAILING gate is not promoted —
// it stays pending for a retry, and nothing is written.
func TestNoPromoteOnFailingGate(t *testing.T) {
	tk := mkTaskFile(t, t.TempDir(), "0001", task.StatusPending)
	res, err := Reconcile(tk, Signals{TestRan: true, TestPassed: false})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Action != ActionUnchanged || res.To != task.StatusPending || res.Wrote {
		t.Errorf("res = %+v, want Unchanged pending, Wrote=false", res)
	}
}

// TestNoPromoteWhenGateDidNotRun: a non-code phase / non-clean invocation leaves
// TestRan=false; a passing-looking TestPassed must not promote without TestRan.
func TestNoPromoteWhenGateDidNotRun(t *testing.T) {
	tk := mkTaskFile(t, t.TempDir(), "0001", task.StatusPending)
	res, err := Reconcile(tk, Signals{TestRan: false, TestPassed: true})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Action != ActionUnchanged || res.Wrote {
		t.Errorf("res = %+v, want Unchanged (gate did not run), Wrote=false", res)
	}
}

// TestNormalizesStuckActive: the agent set active while working but never flipped on
// exit (e.g. a killed loop). Active is not selectable (Next returns only pending),
// so the harness resets it to pending — and persists that.
func TestNormalizesStuckActive(t *testing.T) {
	tk := mkTaskFile(t, t.TempDir(), "0001", task.StatusActive)
	res, err := Reconcile(tk, Signals{TestRan: false})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Action != ActionNormalized || res.To != task.StatusPending || !res.Wrote {
		t.Errorf("res = %+v, want Normalized pending, Wrote=true", res)
	}
	if got := onDisk(t, tk); got != task.StatusPending {
		t.Errorf("on-disk status = %q, want pending (normalization must persist)", got)
	}
}

// TestActivePromotesOverNormalize: a passing gate beats normalization — an active
// task whose tests pass becomes done, not pending.
func TestActivePromotesOverNormalize(t *testing.T) {
	tk := mkTaskFile(t, t.TempDir(), "0001", task.StatusActive)
	res, err := Reconcile(tk, Signals{TestRan: true, TestPassed: true})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Action != ActionPromoted || res.To != task.StatusDone {
		t.Errorf("res = %+v, want Promoted done", res)
	}
}
