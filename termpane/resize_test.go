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
