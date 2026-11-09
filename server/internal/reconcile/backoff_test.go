package reconcile

import (
	"testing"
	"time"
)

func TestBackoffDoublesToItsCeiling(t *testing.T) {
	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{
		{-1, 2 * time.Second},
		{0, 2 * time.Second},
		{1, 4 * time.Second},
		{7, 256 * time.Second},
		{8, 5 * time.Minute},
		{62, 5 * time.Minute},
		{200, 5 * time.Minute},
	} {
		if got := Backoff(DefaultBackoffBase, DefaultBackoffMax, tc.failures); got != tc.want {
			t.Errorf("Backoff(%d) = %s, want %s", tc.failures, got, tc.want)
		}
	}
}
