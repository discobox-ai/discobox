package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
)

// One decimal below ten and none above, so "1.2 GiB" and "15 GiB" both fit the
// same column.
func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2_483_027_968, "2.3 GiB"},
		{15_264_268_288, "14 GiB"},
	} {
		if got := humanBytes(tc.n); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// The age column ranks rows by recency; it is not there to time them, so one
// unit is enough.
func TestSince(t *testing.T) {
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
		if got := since(now.Add(-tc.ago), now); got != tc.want {
			t.Errorf("since(%v) = %q, want %q", tc.ago, got, tc.want)
		}
	}
	if got := since(time.Time{}, now); got != "" {
		t.Errorf("a sandbox never touched has no age, got %q", got)
	}
}

// A background painted across a row that carries its own colors has to be
// re-asserted after every reset, or it would stop at the first styled span.
func TestHighlightSurvivesEmbeddedResets(t *testing.T) {
	st := newStyles(true)
	row := "plain " + st.add.Render("+142") + " more"
	out := highlight(st, row, colSelectedBG)

	bg := "\x1b[48;5;" + colSelectedBG + "m"
	if !strings.HasPrefix(out, bg) {
		t.Fatalf("highlight should open with the background: %q", out)
	}
	// One for the opening, and one after every reset the row carries — lipgloss
	// writes the short spelling, so counting only the long one would miss them.
	resets := strings.Count(out, ansiResetShort) + strings.Count(out, ansiReset)
	if n := strings.Count(out, bg); n <= resets-1 {
		t.Fatalf("the background should be re-asserted after each reset, found %d for %d resets in %q", n, resets, out)
	}
}

// Without color there is no background to paint, and the row is left alone.
func TestHighlightIsANoOpWithoutColour(t *testing.T) {
	st := newStyles(false)
	if got := highlight(st, "plain row", colSelectedBG); got != "plain row" {
		t.Fatalf("highlight = %q, want the row untouched", got)
	}
}

// A column is a fixed number of display cells whatever is put in it.
func TestPadAndTruncateMeasureCells(t *testing.T) {
	if got := pad("abc", 6); got != "abc   " {
		t.Errorf("pad = %q", got)
	}
	if got := pad("abcdefgh", 4); got != "abc…" {
		t.Errorf("pad = %q", got)
	}
	st := newStyles(true)
	styled := st.add.Render("+142")
	if got := lipgloss.Width(padANSI(styled, 9)); got != 9 {
		t.Errorf("padANSI width = %d, want 9", got)
	}
}

// The command preview only quotes what a shell would otherwise split or
// interpret: a preview full of needless quotes is one you stop trusting.
func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"":                  "''",
		"codex":             "codex",
		"/src/disco2@main":  "/src/disco2@main",
		"fix the reaper":    "'fix the reaper'",
		"it's":              `'it'\''s'`,
		"--include-dirty=1": "--include-dirty=1",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
