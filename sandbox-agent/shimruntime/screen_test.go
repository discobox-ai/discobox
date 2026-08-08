package shimruntime

import (
	"strings"
	"testing"
)

func TestScreenSnapshotRepaintsContentAndModes(t *testing.T) {
	s := newScreenBuffer(24, 80, DefaultScrollbackLines)
	// A running TUI enables mouse tracking and bracketed paste, hides the cursor,
	// then draws.
	s.write([]byte("\x1b[?1000h\x1b[?1006h\x1b[?2004h\x1b[?25l"))
	s.write([]byte("hello\r\nworld"))

	snap := string(s.snapshot())

	for _, want := range []string{
		"\x1b[?1h",    // cursor keys default-reset is not emitted (never toggled)
		"\x1b[?1000h", // mouse normal restored
		"\x1b[?1006h", // mouse SGR restored
		"\x1b[?2004h", // bracketed paste restored
		"\x1b[?25l",   // cursor hidden restored
		"hello",
		"world",
	} {
		if want == "\x1b[?1h" {
			// Sanity: mode 1 was never toggled, so it must NOT be restored.
			if strings.Contains(snap, want) {
				t.Fatalf("snapshot restored an untouched mode %q:\n%q", want, snap)
			}
			continue
		}
		if !strings.Contains(snap, want) {
			t.Fatalf("snapshot missing %q:\n%q", want, snap)
		}
	}
	// Primary screen: alt-screen must be left, not entered.
	if !strings.Contains(snap, "\x1b[?1049l") {
		t.Fatalf("snapshot should leave alt screen on primary:\n%q", snap)
	}
	// Cursor is repositioned after "world" (row 2, col 6, 1-based).
	if !strings.Contains(snap, "\x1b[2;6H") {
		t.Fatalf("snapshot missing cursor reposition:\n%q", snap)
	}
}

func TestScreenSnapshotIncludesScrollback(t *testing.T) {
	s := newScreenBuffer(3, 20, DefaultScrollbackLines)
	for i := 0; i < 10; i++ {
		s.write([]byte("row"))
		s.write([]byte{'0' + byte(i)})
		s.write([]byte("\r\n"))
	}
	snap := string(s.snapshot())
	// Early rows have scrolled off the 3-line screen into scrollback and must
	// still appear in the repaint.
	if !strings.Contains(snap, "row0") || !strings.Contains(snap, "row1") {
		t.Fatalf("snapshot missing scrollback rows:\n%q", snap)
	}
	// Clears the client's scrollback before repainting so re-attach is clean.
	if !strings.Contains(snap, "\x1b[3J") {
		t.Fatalf("snapshot should clear client scrollback:\n%q", snap)
	}
}

func TestScreenSnapshotAltScreenOmitsScrollback(t *testing.T) {
	s := newScreenBuffer(3, 20, DefaultScrollbackLines)
	s.write([]byte("scrollme\r\nmore\r\nlines\r\nhere\r\n"))
	s.write([]byte("\x1b[?1049h\x1b[H\x1b[2J")) // enter alt screen, clear
	s.write([]byte("ALTVIEW"))

	snap := string(s.snapshot())
	if !strings.Contains(snap, "\x1b[?1049h") {
		t.Fatalf("alt-screen snapshot should enter alt screen:\n%q", snap)
	}
	if !strings.Contains(snap, "ALTVIEW") {
		t.Fatalf("alt-screen snapshot missing alt content:\n%q", snap)
	}
	if strings.Contains(snap, "\x1b[3J") {
		t.Fatalf("alt-screen snapshot must not clear/emit scrollback:\n%q", snap)
	}
	if strings.Contains(snap, "scrollme") {
		t.Fatalf("alt-screen snapshot must not include primary scrollback:\n%q", snap)
	}
}

func TestModeTrackerCarriesSplitSequence(t *testing.T) {
	var m modeTracker
	// A private-mode set split across two writes must still be recognized.
	m.scan([]byte("\x1b[?10"))
	m.scan([]byte("00h"))
	if !m.set[1000] || !m.seen[1000] {
		t.Fatalf("split mode 1000 not tracked: set=%v seen=%v", m.set, m.seen)
	}

	// A later reset flips it and must be the state emitted.
	m.scan([]byte("\x1b[?1000l"))
	if m.set[1000] {
		t.Fatalf("mode 1000 should be reset")
	}
	if got := m.sequences(); got != "\x1b[?1000l" {
		t.Fatalf("sequences = %q, want reset of 1000", got)
	}
}

func TestModeTrackerIgnoresRegularCSIAndUntracked(t *testing.T) {
	var m modeTracker
	// Regular SGR/CSI and untracked private modes must not be restored.
	m.scan([]byte("\x1b[38;5;1m\x1b[2J\x1b[?7l\x1b[?47h"))
	if got := m.sequences(); got != "" {
		t.Fatalf("sequences = %q, want empty (no tracked modes toggled)", got)
	}
}

// A program announces its title once, at startup, the same as its modes. A
// client that attached afterwards never saw it, so the snapshot carries it.
func TestScreenSnapshotRestoresTheTitle(t *testing.T) {
	s := newScreenBuffer(24, 80, DefaultScrollbackLines)
	s.write([]byte("\x1b]2;claude — src/disco2\a"))
	s.write([]byte("working"))

	snap := string(s.snapshot())
	if !strings.Contains(snap, "\x1b]2;claude — src/disco2\a") {
		t.Fatalf("snapshot missing the title:\n%q", snap)
	}

	// The last one set wins, and OSC 0 sets both the title and the icon name.
	s.write([]byte("\x1b]0;done\x1b\\"))
	snap = string(s.snapshot())
	if strings.Contains(snap, "claude") {
		t.Fatalf("snapshot carried a title that was replaced:\n%q", snap)
	}
	if !strings.Contains(snap, "\x1b]2;done\a") || !strings.Contains(snap, "\x1b]1;done\a") {
		t.Fatalf("snapshot missing the title and icon name:\n%q", snap)
	}
}

// A title that was never set writes nothing: an empty one would clear whatever
// the client's own terminal had, which is worse than leaving it alone.
func TestScreenSnapshotOmitsATitleNeverSet(t *testing.T) {
	s := newScreenBuffer(24, 80, DefaultScrollbackLines)
	s.write([]byte("hello"))

	if snap := string(s.snapshot()); strings.Contains(snap, "\x1b]") {
		t.Fatalf("snapshot should carry no OSC at all:\n%q", snap)
	}
}

// A title carrying a control character would end the sequence early and leave
// the rest of it printing on the client's screen as text, and one carrying a
// novel would be paid for on every attach.
func TestScreenSnapshotSanitizesTheTitle(t *testing.T) {
	s := newScreenBuffer(24, 80, DefaultScrollbackLines)
	// The emulator ends the OSC at the BEL, so the control character has to be
	// planted in what it hands back rather than in the sequence.
	s.title = "safe\x1b]2;evil\aalso"
	if got := string(s.snapshot()); !strings.Contains(got, "\x1b]2;safe\a") || strings.Contains(got, "evil") {
		t.Fatalf("snapshot should stop at the control character:\n%q", got)
	}

	s.title = strings.Repeat("x", maxOSCString+50)
	snap := string(s.snapshot())
	title := snap[strings.Index(snap, "\x1b]2;")+4:]
	if got := len(title[:strings.IndexByte(title, '\a')]); got != maxOSCString {
		t.Fatalf("title is %d bytes, want it capped at %d", got, maxOSCString)
	}
}
