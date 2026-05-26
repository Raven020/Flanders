package guardrail

import "testing"

func TestChanged(t *testing.T) {
	tests := []struct {
		name          string
		files, status bool
		want          bool
	}{
		{"neither — a stall loop", false, false, false},
		{"files only", true, false, true},
		{"status only", false, true, true},
		{"both", true, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Changed(tc.files, tc.status); got != tc.want {
				t.Errorf("Changed(%v, %v) = %v, want %v", tc.files, tc.status, got, tc.want)
			}
		})
	}
}

func TestStallStep(t *testing.T) {
	tests := []struct {
		name    string
		prev    int
		changed bool
		want    int
	}{
		{"no change increments from zero", 0, false, 1},
		{"no change increments mid-streak", 2, false, 3},
		{"change resets a streak to zero", 2, true, 0},
		{"change keeps zero at zero", 0, true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := StallStep(tc.prev, tc.changed); got != tc.want {
				t.Errorf("StallStep(%d, %v) = %d, want %d", tc.prev, tc.changed, got, tc.want)
			}
		})
	}
}

func TestStallTripped(t *testing.T) {
	tests := []struct {
		name       string
		count, n   int
		wantTripped bool
	}{
		{"below limit", 2, 3, false},
		{"at limit trips", 3, 3, true},
		{"above limit stays tripped", 4, 3, true},
		{"zero count never trips", 0, 3, false},
		{"n=0 disables the guardrail", 5, 0, false},
		{"negative n disables (defensive)", 5, -1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := StallTripped(tc.count, tc.n); got != tc.wantTripped {
				t.Errorf("StallTripped(%d, %d) = %v, want %v", tc.count, tc.n, got, tc.wantTripped)
			}
		})
	}
}

// TestStallSequence exercises the audit-required pair (plan task 3.9): the counter
// increments across consecutive no-change loops and trips at exactly stall_n, then a
// single progress loop resets the streak so the guardrail no longer trips. This is
// the end-to-end stall lifecycle the orchestrator drives.
func TestStallSequence(t *testing.T) {
	const n = 3
	count := 0

	// Three consecutive no-change loops: 1, 2, 3 — trips on the third.
	for i := 1; i <= n; i++ {
		count = StallStep(count, Changed(false, false))
		if count != i {
			t.Fatalf("after %d no-change loops: count = %d, want %d", i, count, i)
		}
		tripped := StallTripped(count, n)
		wantTripped := i >= n
		if tripped != wantTripped {
			t.Fatalf("after %d no-change loops: tripped = %v, want %v", i, tripped, wantTripped)
		}
	}

	// A loop that touches files resets the streak; the guardrail is no longer tripped.
	count = StallStep(count, Changed(true, false))
	if count != 0 {
		t.Fatalf("after a file-change loop: count = %d, want 0 (reset)", count)
	}
	if StallTripped(count, n) {
		t.Fatal("after reset the stall guardrail should not be tripped")
	}

	// A status-only change also counts as progress and keeps the streak clear.
	count = StallStep(count, Changed(false, true))
	if count != 0 {
		t.Fatalf("after a status-change loop: count = %d, want 0", count)
	}
}

func TestMaxIterationsReached(t *testing.T) {
	tests := []struct {
		name      string
		iter, max int
		want      bool
	}{
		{"below cap", 99, 100, false},
		{"at cap is inclusive", 100, 100, true},
		{"above cap", 101, 100, true},
		{"max=0 disables the cap", 1000, 0, false},
		{"fresh phase under cap", 0, 100, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MaxIterationsReached(tc.iter, tc.max); got != tc.want {
				t.Errorf("MaxIterationsReached(%d, %d) = %v, want %v", tc.iter, tc.max, got, tc.want)
			}
		})
	}
}

func TestDone(t *testing.T) {
	tests := []struct {
		name                              string
		testPassed, allTasksDone, stalled bool
		want                              bool
	}{
		{"all conditions met", true, true, false, true},
		{"tests failed blocks done", false, true, false, false},
		{"tasks remaining blocks done", true, false, false, false},
		{"a stall in effect blocks done", true, true, true, false},
		{"nothing met", false, false, false, false},
		{"stalled with everything else met still not done", true, true, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Done(tc.testPassed, tc.allTasksDone, tc.stalled); got != tc.want {
				t.Errorf("Done(%v, %v, %v) = %v, want %v",
					tc.testPassed, tc.allTasksDone, tc.stalled, got, tc.want)
			}
		})
	}
}
