package proxy

import (
	"testing"
	"time"
)

func TestDefaultRetentionIsTwoDays(t *testing.T) {
	if DefaultRetention != 48*time.Hour {
		t.Fatalf("DefaultRetention = %s, want 48h", DefaultRetention)
	}
	if got := DefaultConfig().Recording.Retention; got != DefaultRetention {
		t.Fatalf("DefaultConfig() retention = %s, want %s", got, DefaultRetention)
	}
}

func TestSweepIntervalTracksHalfTheWindowWithinBounds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retention time.Duration
		want      time.Duration
	}{
		{"half the window", 90 * time.Minute, 45 * time.Minute},
		{"clamped up so a short window is not a busy loop", 30 * time.Second, time.Minute},
		{"clamped down so the default sweeps hourly", DefaultRetention, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SweepInterval(tc.retention); got != tc.want {
				t.Fatalf("SweepInterval(%s) = %s, want %s", tc.retention, got, tc.want)
			}
		})
	}
}

func TestNegativeRetentionIsRejected(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Recording.Retention = -time.Hour
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted a negative retention window")
	}
}

// Zero is how an embedder that manages the audit database itself opts out, so
// it must leave the sweeper unstarted rather than sweep against a zero cutoff —
// which would delete everything ever recorded.
func TestZeroRetentionSweepsNothing(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Recording.Retention = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() rejected an opted-out retention: %v", err)
	}
	server := &Server{cfg: cfg, closed: make(chan struct{})}
	server.startRetentionSweeper()
	// Nothing was started, so nothing is waiting to be waited on.
	done := make(chan struct{})
	go func() {
		server.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a zero retention window started a sweeper")
	}
}
