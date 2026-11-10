package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
)

// A background painted across a row that carries its own colors has to be
// re-asserted after every reset, or it would stop at the first styled span.
func TestHighlightSurvivesEmbeddedResets(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	st := newStyles(false)
	if got := highlight(st, "plain row", colSelectedBG); got != "plain row" {
		t.Fatalf("highlight = %q, want the row untouched", got)
	}
}

// A column is a fixed number of display cells whatever is put in it.
func TestPadAndTruncateMeasureCells(t *testing.T) {
	t.Parallel()
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

// A message too long for its row keeps both ends: an error's start says what
// failed and its end says why.
func TestTruncateMiddleKeepsBothEnds(t *testing.T) {
	t.Parallel()
	msg := "cannot create the discobox: pull image: failed to register layer: no space left on device"
	got := truncateMiddle(msg, 60)
	if w := lipgloss.Width(got); w != 60 {
		t.Fatalf("truncateMiddle = %q, %d cells wide, want 60", got, w)
	}
	if !strings.HasPrefix(got, "cannot create") || !strings.HasSuffix(got, "no space left on device") || !strings.Contains(got, "…") {
		t.Fatalf("truncateMiddle = %q, want the start and the cause either side of an ellipsis", got)
	}

	if got := truncateMiddle("short", 10); got != "short" {
		t.Errorf("truncateMiddle = %q, want text that fits left alone", got)
	}
	if got := truncateMiddle("abcdef", 1); got != "…" {
		t.Errorf("truncateMiddle = %q, want the ellipsis alone in one cell", got)
	}
	if got := truncateMiddle("abcdef", 0); got != "" {
		t.Errorf("truncateMiddle = %q, want nothing in no room", got)
	}
	// Wide characters are measured in cells, so the result never overruns.
	for w := 1; w <= 12; w++ {
		if got := truncateMiddle("文字化けの原因はここ", w); lipgloss.Width(got) > w {
			t.Errorf("truncateMiddle at %d = %q, %d cells wide", w, got, lipgloss.Width(got))
		}
	}
}

// Nothing a card wraps is lost to its width: a word wider than the line — a
// URL, a header value — is broken where the line runs out rather than cut, and
// joining the rows back gives the text back.
func TestWrapLosesNothing(t *testing.T) {
	t.Parallel()
	url := "https://api.github.com/repos/discobox-ai/discobox/pulls?state=open&per_page=100&page=2"
	lines := wrap("url:   "+url, 30)
	for _, line := range lines {
		if w := lipgloss.Width(line); w > 30 {
			t.Fatalf("row %q is %d cells, past the width", line, w)
		}
	}
	var got strings.Builder
	for i, line := range lines {
		if i > 0 {
			line = strings.TrimPrefix(line, "       ")
		}
		got.WriteString(line)
	}
	if got.String() != "url:   "+url {
		t.Fatalf("rows %q do not give the line back", lines)
	}
}

// A wrapped row hangs under the line's second column — the value of a
// `label:  value` field, the description of a key — or under its indentation
// when it has none, and keeps the spacing inside it.
func TestWrapHangsUnderTheValue(t *testing.T) {
	t.Parallel()
	got := wrap("method:   GET a request whose url runs long", 24)
	want := []string{"method:   GET a request", "          whose url runs", "          long"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("wrap = %q, want %q", got, want)
	}
	got = wrap(`    "tool_input": "go test ./... and more"`, 24)
	want = []string{`    "tool_input": "go`, `    test ./... and more"`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("wrap = %q, want %q", got, want)
	}
}

// A megabyte on one line — a response body with no spaces in it — is broken
// in one pass. Breaking it by re-cutting the rest of the word for each row
// took minutes, and the card froze the window while it did.
func TestWrapBreaksAMegabyteLineInOnePass(t *testing.T) {
	t.Parallel()
	line := strings.Repeat("0123456789abcdef", 1<<16)
	start := time.Now()
	rows := wrap("body:  "+line, 80)
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("wrapping a megabyte took %s", took)
	}
	var got strings.Builder
	for i, row := range rows {
		if w := lipgloss.Width(row); w > 80 {
			t.Fatalf("row %d is %d cells", i, w)
		}
		if i > 0 {
			row = strings.TrimPrefix(row, "       ")
		}
		got.WriteString(row)
	}
	if got.String() != "body:  "+line {
		t.Fatal("the rows do not give the line back")
	}
}

// A card lays its body out once per width, not once per frame.
func TestADialogWrapsItsBodyOncePerWidth(t *testing.T) {
	t.Parallel()
	d := textDialog("t", strings.Repeat("word ", 1000))
	first := d.bodyAt(40)
	if again := d.bodyAt(40); &again[0] != &first[0] {
		t.Fatal("the same body at the same width should not be laid out again")
	}
	if wider := d.bodyAt(60); &wider[0] == &first[0] {
		t.Fatal("a new width should lay the body out again")
	}
	d.body = "changed"
	if changed := d.bodyAt(60); changed[0] != "changed" {
		t.Fatal("a new body should be laid out again")
	}
}
