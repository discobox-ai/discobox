package sandboxcreate

import (
	"testing"
	"time"
)

// The bug this exists for: a wait bounded by total elapsed time gives up on a
// long pull that is going perfectly well.
func TestStallClockSurvivesAWaitLongerThanItsWindow(t *testing.T) {
	clock := NewStallClock(40 * time.Millisecond)
	// Four windows' worth of waiting, reporting progress throughout, the way a
	// pull restates its byte counts.
	for range 4 {
		time.Sleep(30 * time.Millisecond)
		if clock.Expired() {
			t.Fatal("gave up on a wait that was still reporting progress")
		}
		clock.Progressed()
	}
	if clock.Expired() {
		t.Fatal("expired despite continuous progress")
	}
}

// And it still has to end: silence is what spends it.
func TestStallClockExpiresOnSilence(t *testing.T) {
	clock := NewStallClock(20 * time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	if !clock.Expired() {
		t.Fatal("a wait with nothing happening never gave up")
	}
}

func TestStallClockReportsItsWindow(t *testing.T) {
	if got := NewStallClock(time.Minute).Window(); got != time.Minute {
		t.Fatalf("Window() = %s, want 1m", got)
	}
}
