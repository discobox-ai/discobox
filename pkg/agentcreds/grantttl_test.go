package agentcreds_test

import (
	"math"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
)

// What arrives from inside a sandbox is not a lifetime until it is checked. The
// ceiling is what keeps the conversion from overflowing: 18446744074 seconds
// multiplied out wraps to 290.448384ms, which is positive, rounds to zero whole
// seconds, and would read as forever everywhere a lifetime is shown.
func TestAskedGrantTTLTakesOnlyALifetimeAnAgentMayAskFor(t *testing.T) {
	for _, tc := range []struct {
		seconds int64
		want    time.Duration
	}{
		{0, 0},
		{-1, 0},
		{3600, time.Hour},
		{int64(agentcreds.MaxGrantTTLSeconds), 30 * 24 * time.Hour},
		{int64(agentcreds.MaxGrantTTLSeconds) + 1, 0},
		{18446744074, 0},
		{math.MaxInt64, 0},
	} {
		if got := agentcreds.AskedGrantTTL(tc.seconds); got != tc.want {
			t.Errorf("AskedGrantTTL(%d) = %v, want %v", tc.seconds, got, tc.want)
		}
	}
}
