package sandbox

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A phase entered once and held for minutes reads, to anyone judging by the
// record's age, as a phase that was abandoned. That is what made a booting VM
// look stopped: the narration blanked back to "waiting for a pool to take it",
// and placement gave up with "no sandbox capacity" while the boot was going
// fine.
func TestHoldKeepsRestatingThePhase(t *testing.T) {
	defer shortHeartbeat(t)()

	var mu sync.Mutex
	var reports []PoolProvisionPhase
	reporter := PoolProgressReporter(func(_ context.Context, _ string, progress PoolProvisionProgress) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, progress.Phase)
	})

	release := reporter.Hold(t.Context(), "pool_1", PoolPhaseStartingVM)
	// Long enough for several heartbeats.
	deadline := time.Now().Add(3 * poolPhaseHeartbeat)
	for time.Now().Before(deadline) {
		mu.Lock()
		enough := len(reports) >= 3
		mu.Unlock()
		if enough {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	release()

	mu.Lock()
	got := append([]PoolProvisionPhase(nil), reports...)
	mu.Unlock()
	if len(got) < 3 {
		t.Fatalf("got %d reports in %s, want the phase restated while it was held", len(got), 3*poolPhaseHeartbeat)
	}
	for _, phase := range got {
		if phase != PoolPhaseStartingVM {
			t.Fatalf("reported %q, want every restatement to be the held phase", phase)
		}
	}
}

// Releasing stops it, or a finished phase would go on claiming to be current.
func TestHoldStopsOnRelease(t *testing.T) {
	defer shortHeartbeat(t)()

	var mu sync.Mutex
	count := 0
	reporter := PoolProgressReporter(func(context.Context, string, PoolProvisionProgress) {
		mu.Lock()
		defer mu.Unlock()
		count++
	})

	release := reporter.Hold(t.Context(), "pool_1", PoolPhaseWaitingForDocker)
	release()
	mu.Lock()
	afterRelease := count
	mu.Unlock()

	time.Sleep(2 * poolPhaseHeartbeat)
	mu.Lock()
	defer mu.Unlock()
	if count != afterRelease {
		t.Fatalf("kept reporting after release: %d then %d", afterRelease, count)
	}
	// Releasing twice is safe: the callers defer it and also call it early.
	release()
}

// A driver with no reporter says nothing, and holding nothing is still safe.
func TestHoldToleratesNoReporter(t *testing.T) {
	var reporter PoolProgressReporter
	release := reporter.Hold(t.Context(), "pool_1", PoolPhaseStartingVM)
	release()
}

// shortHeartbeat makes a held phase restate itself fast enough to assert on.
func shortHeartbeat(t *testing.T) func() {
	t.Helper()
	previous := poolPhaseHeartbeat
	poolPhaseHeartbeat = 10 * time.Millisecond
	return func() { poolPhaseHeartbeat = previous }
}
