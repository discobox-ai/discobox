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
