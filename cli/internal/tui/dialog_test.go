package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// A dialog takes most of a window with room to spare, and all of one without.
func TestADialogTakesMostOfTheWindow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window int
		want   int
	}{
		{"wide", 120, 108},
		{"at the threshold", 80, 72},
		{"below the threshold, so all of it", 60, 60},
		{"narrower than the minimum, so still all of it", 40, 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dialogWidth(tc.window); got != tc.want {
				t.Fatalf("dialogWidth(%d) = %d, want %d", tc.window, got, tc.want)
			}
		})
	}
}

// Whatever the policy works out to, it can never be wider than the terminal:
// a dialog that overflows is one with its right edge off screen.
func TestADialogNeverOutgrowsTheWindow(t *testing.T) {
	st := newStyles(false)
	dialog := textDialog("Title", strings.Repeat("a word ", 200))
	// From the narrowest box there is: the border and padding alone are six
	// columns, so below seven there is no box to draw, only its chrome.
	for window := dialogChromeWidth + 1; window <= 200; window++ {
		if got := dialogWidth(window); got > window {
			t.Fatalf("dialogWidth(%d) = %d, wider than the window", window, got)
		}
		if got := lipgloss.Width(dialog.view(st, window, 40)); got > window {
			t.Fatalf("a dialog rendered at %d columns came out %d wide", window, got)
		}
	}
}

// The box's outside extent is what the policy asked for, so the sizing means
// what it says rather than what the border leaves of it.
func TestADialogIsDrawnAtTheWidthItWasGiven(t *testing.T) {
	st := newStyles(false)
	dialog := textDialog("Title", "a short body")
	for _, window := range []int{120, 100, 80, 60} {
		if got := lipgloss.Width(dialog.view(st, window, 40)); got != dialogWidth(window) {
			t.Fatalf("at %d columns the box is %d wide, want %d", window, got, dialogWidth(window))
		}
	}
}

// A dialog is the only thing on screen, so it sits in the middle of it.
func TestADialogIsCentered(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	send(t, m, sizeMsg(120, 40), key("f1"))

	lines := strings.Split(rawFrame(m), "\n")
	first, last := -1, -1
	for i, line := range lines {
		if strings.TrimSpace(line) != "" {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		t.Fatal("the help dialog drew nothing")
	}
	// Centered vertically: the blank rows above and below differ by at most one,
	// which is what an odd number of spare rows leaves.
	above, below := first, len(lines)-1-last
	if diff := above - below; diff > 1 || diff < -1 {
		t.Fatalf("dialog has %d rows above and %d below, want it centered:\n%s", above, below, rawFrame(m))
	}
	// And horizontally, by the same measure on the widest row.
	widest := ""
	for _, line := range lines {
		if lipgloss.Width(line) > lipgloss.Width(widest) {
			widest = line
		}
	}
	left := lipgloss.Width(widest) - lipgloss.Width(strings.TrimLeft(widest, " "))
	right := 120 - lipgloss.Width(strings.TrimRight(widest, " "))
	if diff := left - right; diff > 1 || diff < -1 {
		t.Fatalf("dialog has %d columns left and %d right, want it centered", left, right)
	}
}

// Centering must not make the frame bigger than the terminal.
func TestACenteredDialogStillFitsTheTerminal(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	for _, size := range [][2]int{{120, 40}, {100, 24}, {80, 20}, {60, 16}} {
		send(t, m, sizeMsg(size[0], size[1]))
		if m.dialog == nil {
			send(t, m, key("f1"))
		}
		frame := rawFrame(m)
		lines := strings.Split(frame, "\n")
		if len(lines) != size[1] {
			t.Fatalf("frame at %dx%d is %d rows, want %d", size[0], size[1], len(lines), size[1])
		}
		for i, line := range lines {
			if got := lipgloss.Width(line); got > size[0] {
				t.Fatalf("row %d at %dx%d is %d columns, want at most %d", i, size[0], size[1], got, size[0])
			}
		}
	}
}

// The body grows with the window: a card that scrolled in a short terminal
// should not still be scrolling in a tall one.
func TestALongBodyUsesTheHeightItIsGiven(t *testing.T) {
	st := newStyles(false)
	body := strings.TrimRight(strings.Repeat("a line\n", 60), "\n")
	dialog := textDialog("Title", body)

	short := strings.Count(dialog.view(st, 100, 20), "\n")
	dialog.offset = 0
	tall := strings.Count(dialog.view(st, 100, 60), "\n")
	if tall <= short {
		t.Fatalf("body rows: %d in a 20-row window, %d in a 60-row one — want the taller window to show more", short, tall)
	}
}

// searchable is a scrolling text dialog with a body long enough to have
// something in it that is not already on screen.
func searchable() *dialog {
	var lines []string
	for i := range 60 {
		lines = append(lines, "line "+itoa(i))
	}
	lines[17] = "the leader detaches"
	lines[42] = "the leader quits"
	return textDialog("Title", strings.Join(lines, "\n"))
}

// draw renders the dialog, which is what counts the matches: the body is
// wrapped to the window, so how many lines hold a word is only known here.
func draw(d *dialog) string { return d.view(newStyles(false), 100, 24) }

// / searches the body, and the tally says what it found while it is still
// being typed.
func TestSlashSearchesAScrollingBody(t *testing.T) {
	d := searchable()
	draw(d)
	send := func(spec string) { d.update(key(spec)) }

	send("/")
	if !d.typing {
		t.Fatal("/ did not open the search line")
	}
	for _, r := range "leader" {
		send(string(r))
	}
	if got := draw(d); !strings.Contains(got, "/leader") {
		t.Fatalf("the search line is not on the frame:\n%s", got)
	}
	if d.query != "leader" || d.matches != 2 {
		t.Fatalf("query %q with %d matches, want \"leader\" with 2", d.query, d.matches)
	}
	if !strings.Contains(draw(d), "the leader detaches") {
		t.Fatal("the body did not scroll to the first match")
	}
}

// Enter puts the line away and keeps the search: n and N walk the matches, and
// the footer still says what was searched for.
func TestNAndNWalkTheMatches(t *testing.T) {
	d := searchable()
	draw(d)
	for _, spec := range []string{"/", "l", "e", "a", "d", "e", "r", "enter"} {
		d.update(key(spec))
	}
	if d.typing {
		t.Fatal("Enter left the search line open")
	}
	d.update(key("n"))
	if got := draw(d); !strings.Contains(got, "the leader quits") || !strings.Contains(got, "/leader") {
		t.Fatalf("n did not go to the second match:\n%s", got)
	}
	if d.match != 1 {
		t.Fatalf("match = %d, want the second", d.match)
	}
	// Past the end is the first again: two matches are a ring, not a list with
	// a wall at each end.
	d.update(key("n"))
	draw(d)
	if d.match != 0 {
		t.Fatalf("match = %d after walking past the end, want the first", d.match)
	}
	d.update(key("N"))
	draw(d)
	if d.match != 1 {
		t.Fatalf("match = %d after N from the first, want the last", d.match)
	}
}

// Esc belongs to the search line while one is open: it abandons the search and
// puts the body back where it was, rather than closing the dialog.
func TestEscAbandonsTheSearchAndKeepsYourPlace(t *testing.T) {
	d := searchable()
	draw(d)
	for range 10 {
		d.update(key("down"))
	}
	draw(d)
	was := d.offset

	for _, spec := range []string{"/", "l", "e", "a", "d", "e", "r"} {
		d.update(key(spec))
	}
	draw(d)
	if d.offset == was {
		t.Fatal("the search did not move the body")
	}
	if _, closed := d.update(key("esc")); closed {
		t.Fatal("Esc closed the dialog instead of the search line")
	}
	draw(d)
	if d.query != "" || d.matches != 0 {
		t.Fatalf("the search survived Esc: %q with %d matches", d.query, d.matches)
	}
	if d.offset != was {
		t.Fatalf("offset = %d after abandoning the search, want %d", d.offset, was)
	}
	// With no search open, Esc is the way out again.
	if _, closed := d.update(key("esc")); !closed {
		t.Fatal("Esc did not close the dialog")
	}
}

// Backspacing past the start of the query is the same as never having asked.
func TestBackspacingOutOfTheSearchEndsIt(t *testing.T) {
	d := searchable()
	draw(d)
	for _, spec := range []string{"/", "l", "backspace", "backspace"} {
		d.update(key(spec))
	}
	if d.typing || d.query != "" {
		t.Fatalf("typing = %v, query = %q, want the search gone", d.typing, d.query)
	}
}

// A body that fits its window does not offer a search: everything in it is
// already on screen.
func TestAShortBodyDoesNotOfferASearch(t *testing.T) {
	st := newStyles(false)
	short := textDialog("Title", "one line").view(st, 100, 24)
	if strings.Contains(short, "/ search") {
		t.Fatalf("a body that fits offered a search:\n%s", short)
	}
	if !strings.Contains(draw(searchable()), "/ search") {
		t.Fatal("a body that scrolls did not offer a search")
	}
}

// The matches are painted, and a painted row is still a row of the box: the
// background is drawn across the width the frame budgeted for it.
func TestASearchedDialogStaysInsideItsBox(t *testing.T) {
	st := newStyles(true)
	for _, window := range []int{120, 100, 80, 60, 40, 20} {
		d := searchable()
		d.view(st, window, 24)
		for _, spec := range []string{"/", "l", "e", "a", "d", "e", "r"} {
			d.update(key(spec))
		}
		for _, line := range strings.Split(d.view(st, window, 24), "\n") {
			if got := lipgloss.Width(line); got > dialogWidth(window) {
				t.Fatalf("at %d columns a searched row is %d wide, want at most %d",
					window, got, dialogWidth(window))
			}
		}
	}
}

// The help is what the search is for: from the window, F1 and / find a key
// without reading down the whole of it.
func TestTheHelpIsSearchable(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	send(t, m, sizeMsg(100, 30), key("f1"), key("/"))
	send(t, m, typeString("vscode")...)
	got := plainFrame(m)
	if !strings.Contains(got, "/vscode") {
		t.Fatalf("the search line is not on the frame:\n%s", got)
	}
	if !strings.Contains(got, "vscode takes neither") {
		t.Fatalf("the help did not scroll to the match:\n%s", got)
	}
}
