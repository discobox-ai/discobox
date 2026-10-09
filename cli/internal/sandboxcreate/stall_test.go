package sandboxcreate

import (
	"testing"
	"testing/synctest"
	"time"
)

// These run in a synctest bubble, whose clock moves only when every goroutine
// in it is blocked, and then exactly as far as the next sleep asks. On the real
// clock a 30ms sleep can overshoot a 40ms window on its own — Windows' timer
// granularity is about 15.6ms — and the test measured the scheduler, not the
// clock (#108).

// The bug this exists for: a wait bounded by total elapsed time gives up on a
// long pull that is going perfectly well.
func TestStallClockSurvivesAWaitLongerThanItsWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := NewStallClock(40 * time.Millisecond)
		// Four windows' worth of waiting, reporting progress throughout, the
		// way a pull restates its byte counts.
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
	})
}

// And it still has to end: silence is what spends it, and any silence past the
// window does.
func TestStallClockExpiresOnSilence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := NewStallClock(20 * time.Millisecond)
		time.Sleep(20*time.Millisecond + time.Nanosecond)
		if !clock.Expired() {
			t.Fatal("a wait with nothing happening never gave up")
		}
	})
}

func TestStallClockReportsItsWindow(t *testing.T) {
	if got := NewStallClock(time.Minute).Window(); got != time.Minute {
		t.Fatalf("Window() = %s, want 1m", got)
	}
}
