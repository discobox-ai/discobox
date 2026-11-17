package poolagent

import (
	"testing"
	"time"
)

// The resolve token's age is read off the wall clock, which is what the
// control plane checks its expiry against. A sleep the monotonic clock never
// saw still makes the token due as soon as the wall clock shows it (#1282).
func TestResolveTokenDue(t *testing.T) {
	minted := time.Date(2026, 11, 17, 4, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		minted time.Time
		now    time.Time
		due    bool
	}{
		{"none written yet", time.Time{}, minted, true},
		{"just minted", minted, minted, false},
		{"short of the refresh age", minted, minted.Add(resolveTokenRefresh - time.Second), false},
		{"at the refresh age", minted, minted.Add(resolveTokenRefresh), true},
		{"expired across a long sleep", minted, minted.Add(8 * time.Hour), true},
		{"clock stepped back", minted, minted.Add(-time.Minute), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveTokenDue(tc.minted, tc.now); got != tc.due {
				t.Fatalf("resolveTokenDue(%v, %v) = %v, want %v", tc.minted, tc.now, got, tc.due)
			}
		})
	}
}
