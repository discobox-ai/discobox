package poolagent

import (
	"testing"
	"time"
)

// The refresher measures a token's age on the wall clock, which is what the
// control plane checks its expiry against. A nap the monotonic clock never
// saw still makes the token due the moment the wall clock shows it.
func TestResolveTokenDue(t *testing.T) {
	minted := time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		minted time.Time
		now    time.Time
		due    bool
	}{
		{"none minted yet", time.Time{}, minted, true},
		{"just minted", minted, minted, false},
		{"short of the refresh", minted, minted.Add(resolveTokenRefresh - time.Second), false},
		{"at the refresh", minted, minted.Add(resolveTokenRefresh), true},
		{"lapsed across a night's sleep", minted, minted.Add(8 * time.Hour), true},
		{"clock stepped back", minted, minted.Add(-time.Minute), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveTokenDue(tc.minted, tc.now); got != tc.due {
				t.Fatalf("resolveTokenDue(%v, %v) = %v, want %v", tc.minted, tc.now, got, tc.due)
			}
		})
	}
}
