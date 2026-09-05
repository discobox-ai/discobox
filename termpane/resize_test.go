package termpane

import "testing"

// A pane that has just shrunk keeps receiving output the far end computed for
// the size it had: the application learns of the resize only when it handles
// its SIGWINCH, and everything already in flight names rows the pane no longer
// has. Margins are the sharp edge — a scroll region set for the taller screen,
// then anything that scrolls inside it, indexes off the end of the emulator's
// buffer, and a panic there ends the whole program rather than one pane.
func TestOutputSizedForTheOldScreenSurvivesAShrink(t *testing.T) {
	m, stream, cmd := attach(t, 80, 24)
	stream.send("before")
	cmd = pump(t, m, cmd, "before")

	m.SetSize(80, 20)

	// DECSTBM for the 24 rows the far end still believes it has, then an
	// insert-line inside that region.
	stream.send("\x1b[1;24r\x1b[6;1H\x1b[Lafter")
	pump(t, m, cmd, "after")
}

// The same race across the other axis: left/right margins for the wider screen
// the far end has not been told it lost.
func TestOutputSizedForTheOldWidthSurvivesAShrink(t *testing.T) {
	m, stream, cmd := attach(t, 80, 24)
	stream.send("before")
	cmd = pump(t, m, cmd, "before")

	m.SetSize(40, 24)

	stream.send("\x1b[?69h\x1b[1;80s\x1b[6;1H\x1b[Lafter")
	pump(t, m, cmd, "after")
}

// A repaint re-asserts the size the far end may no longer be at, and asks for
// the screen back.
//
// The size goes out even though nothing here changed, which is the opposite of
// what SetSize does with the same numbers. The point is that the size in doubt
// is the far end's: another client attached to the same terminal resized it,
// and this pane never saw that happen.
func TestRepaintReassertsTheSizeAndAsksForTheScreen(t *testing.T) {
	m, stream, _ := attach(t, 80, 24)
	before := len(stream.resizes())

	m.Repaint()

	sizes := stream.resizes()
	if len(sizes) != before+1 {
		t.Fatalf("sent %d resizes, want the size re-asserted", len(sizes)-before)
	}
	if sizes[len(sizes)-1] != [2]int{80, 24} {
		t.Fatalf("re-asserted %v, want the pane's own size", sizes[len(sizes)-1])
	}
	if got := stream.repaintCount(); got != 1 {
		t.Fatalf("asked for %d repaints, want 1", got)
	}
}

// A read-only pane asks for nothing. It sends no size to begin with, so there
// is no far end laid out for someone else's window and nothing of its own to
// re-assert.
func TestRepaintOnAReadOnlyPaneSendsNothing(t *testing.T) {
	m, stream, _ := attach(t, 80, 24, WithReadOnly())

	m.Repaint()

	if got := len(stream.resizes()); got != 0 {
		t.Fatalf("sent %d resizes from a read-only pane, want none", got)
	}
	if got := stream.repaintCount(); got != 0 {
		t.Fatalf("asked for %d repaints from a read-only pane, want none", got)
	}
}
