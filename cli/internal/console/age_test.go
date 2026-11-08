package console

import (
	"testing"
	"time"
)

// The age column ranks rows by recency; it is not there to time them, so one
// unit is enough.
func TestSince(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		ago  time.Duration
		want string
	}{
		{10 * time.Second, "now"},
		{2 * time.Minute, "2m"},
		{90 * time.Minute, "1h"},
		{50 * time.Hour, "2d"},
	} {
		if got := Since(now.Add(-tc.ago), now); got != tc.want {
			t.Errorf("Since(%v) = %q, want %q", tc.ago, got, tc.want)
		}
	}
	if got := Since(time.Time{}, now); got != "" {
		t.Errorf("a sandbox never touched has no age, got %q", got)
	}
}

// A column's age reads as English at every size, including the first minute,
// where "now ago" does not.
func TestAgoReadsAsEnglish(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		t    time.Time
		want string
	}{
		{time.Time{}, ""},
		{now.Add(-10 * time.Second), "just now"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-3 * time.Hour), "3h ago"},
		{now.Add(-50 * time.Hour), "2d ago"},
	} {
		if got := Ago(tc.t, now); got != tc.want {
			t.Errorf("Ago(%v) = %q, want %q", now.Sub(tc.t), got, tc.want)
		}
	}
}
