package client

import (
	"strings"
	"testing"
)

// undoAfter feeds output to a fresh display state in the chunks given and
// returns what handing the terminal back would write.
func undoAfter(output ...string) string {
	d := newDisplayState()
	for _, chunk := range output {
		d.observe([]byte(chunk))
	}
	return d.undo()
}

// A remote that turned nothing on is handed back a terminal nothing is written
// to: text, a title and ordinary line endings change no mode. A reset written
// anyway moves the caller's cursor to the top of the screen, because leaving an
// alternate screen that was never entered restores a cursor that was never
// saved, and resetting the scrolling region homes the cursor.
func TestAnUntouchedTerminalIsHandedBackUntouched(t *testing.T) {
	if got := undoAfter("root@host:/# ls\r\nbin  etc\r\n\x1b]0;root@host\x07caf\xc3\xa9\r\n"); got != "" {
		t.Fatalf("undo = %q, want nothing", got)
	}
}

// Every mode a program turns on for itself is turned off, and only that.
func TestWhatARemoteLeavesOnIsTurnedOff(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		undo   string
	}{
		{"the alternate screen", "\x1b[?1049h", "\x1b[?1049l"},
		{"the alternate screen without a saved cursor", "\x1b[?47h", "\x1b[?47l"},
		{"X10 mouse reporting", "\x1b[?9h", "\x1b[?9l"},
		{"mouse click reporting", "\x1b[?1000h", "\x1b[?1000l"},
		{"mouse highlight tracking", "\x1b[?1001h", "\x1b[?1001l"},
		{"mouse drag reporting", "\x1b[?1002h", "\x1b[?1002l"},
		{"mouse motion reporting", "\x1b[?1003h", "\x1b[?1003l"},
		{"focus reporting", "\x1b[?1004h", "\x1b[?1004l"},
		{"SGR mouse encoding", "\x1b[?1006h", "\x1b[?1006l"},
		{"bracketed paste", "\x1b[?2004h", "\x1b[?2004l"},
		{"synchronized output", "\x1b[?2026h", "\x1b[?2026l"},
		{"application cursor keys", "\x1b[?1h", "\x1b[?1l"},
		{"reverse video", "\x1b[?5h", "\x1b[?5l"},
		{"the application keypad as a mode", "\x1b[?66h", "\x1b[?66l"},
		{"color scheme reports", "\x1b[?2031h", "\x1b[?2031l"},
		{"in-band resize reports", "\x1b[?2048h", "\x1b[?2048l"},
		{"a hidden cursor", "\x1b[?25l", "\x1b[?25h"},
		{"autowrap off", "\x1b[?7l", "\x1b[?7h"},
		{"the application keypad", "\x1b=", "\x1b>"},
		{"kitty keyboard pushes", "\x1b[>1u\x1b[>3u", "\x1b[<2u"},
		{"kitty keyboard flags on the caller's entry", "\x1b[=5;1u", "\x1b[=0;1u"},
		{"modifyOtherKeys", "\x1b[>4;2m", "\x1b[>4;0m"},
		{"insert mode", "\x1b[4h", "\x1b[4l"},
		{"a cursor shape", "\x1b[5 q", "\x1b[0 q"},
		{"attributes", "\x1b[1;31m", "\x1b[m"},
		{"line-drawing characters", "\x1b(0", "\x1b(B\x0f"},
		{"a shifted character set", "\x0e", "\x1b(B\x0f"},
		// The margins go back between a cursor save and restore: resetting
		// them homes the cursor.
		{"a scrolling region", "\x1b[2;20r", "\x1b7\x1b[r\x1b8"},
		{"several modes in one sequence", "\x1b[?1000;1006h", "\x1b[?1000l\x1b[?1006l"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := undoAfter(tc.output); got != tc.undo {
				t.Fatalf("undo = %q, want %q", got, tc.undo)
			}
		})
	}
}

// What the remote turned off again is not turned off a second time: a program
// that exited cleanly leaves nothing to undo.
func TestWhatARemoteTurnedOffAgainIsLeftAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
	}{
		{"the alternate screen", "\x1b[?1049h\x1b[?1049l"},
		{"mouse reporting", "\x1b[?1000;1006h\x1b[?1000;1006l"},
		{"a hidden cursor", "\x1b[?25l\x1b[?25h"},
		{"the keypad", "\x1b=\x1b>"},
		{"kitty keyboard pushes", "\x1b[>1u\x1b[>1u\x1b[<2u"},
		{"kitty flags on a pushed entry", "\x1b[>1u\x1b[=5;1u\x1b[<u"},
		{"modifyOtherKeys", "\x1b[>4;2m\x1b[>4m"},
		{"a scrolling region", "\x1b[2;20r\x1b[r"},
		{"a full reset", "\x1b[?1049h\x1b[?1000h\x1b=\x1bc"},
		{"a soft reset", "\x1b[?25l\x1b[3;9r\x1b=\x1b[!p"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := undoAfter(tc.output); got != "" {
				t.Fatalf("undo = %q, want nothing", got)
			}
		})
	}
}

// A sequence split across two writes is still read: output frames end wherever
// the remote's reads did.
func TestASequenceSplitAcrossWritesIsRead(t *testing.T) {
	if got, want := undoAfter("abc\x1b[?10", "49h"), "\x1b[?1049l"; got != want {
		t.Fatalf("undo = %q, want %q", got, want)
	}
}

// The alternate screen keeps its own kitty keyboard stack, so what was pushed
// there is popped before the screen is left — popping it after would take the
// caller's entries off the main screen's stack — and the screen is left before
// anything else is put back, so the rest lands on the screen the caller keeps.
func TestTheAlternateScreenIsUndoneFirstWithItsOwnKeyboardStack(t *testing.T) {
	got := undoAfter("\x1b[>1u\x1b[?1049h\x1b[>1u\x1b[>1u\x1b[?2004h")
	want := "\x1b[<2u\x1b[?1049l\x1b[<1u\x1b[?2004l"
	if got != want {
		t.Fatalf("undo = %q, want %q", got, want)
	}
}

// Nothing handing the terminal back writes may take the caller's screen or
// scrollback, whatever the remote left on.
func TestTheUndoNeverClearsTheCallersScreen(t *testing.T) {
	got := undoAfter("\x1b[?1049h\x1b[?1000h\x1b[2;20r\x1b[1m\x1b(0\x1b[?25l\x1b[>1u\x1b=")
	for _, clear := range []string{"\x1bc", "\x1b[2J", "\x1b[3J"} {
		if strings.Contains(got, clear) {
			t.Fatalf("undo %q clears the screen with %q", got, clear)
		}
	}
}

// Attributes and character sets are reset once the remote has changed them,
// even when it changed them back: a cursor restore brings back the ones saved
// with it, with no sequence in the stream to say so. A progress bar that draws
// its status line between a save and a restore leaves the terminal green here,
// though the last SGR it wrote was a reset.
func TestAttributesACursorRestoreCanBringBackAreReset(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		undo   string
	}{
		{"restored by DECRC", "\x1b[32mdownloading\x1b7\x1b[24;1H\x1b[0mstatus\x1b8", "\x1b[m"},
		{"restored by leaving the alternate screen", "\x1b[1m\x1b(0\x1b[?1049h\x1b[0m\x1b(B", "\x1b[?1049l\x1b[m\x1b(B\x0f"},
		// A soft reset on the alternate screen leaves the main screen's saved
		// cursor, and what leaving the screen restores with it.
		{"restored past a soft reset", "\x1b[1m\x1b(0\x1b[?1049h\x1b[!p", "\x1b[?1049l\x1b[m\x1b(B\x0f"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := undoAfter(tc.output); got != tc.undo {
				t.Fatalf("undo = %q, want %q", got, tc.undo)
			}
		})
	}
}

// A stream cut off inside a sequence leaves the caller's terminal inside it: a
// title swallows the shell's prompt until a BEL happens by, and a control
// sequence takes the prompt's first byte as its final. CAN abandons it, and a
// stream that ended cleanly is not given one.
func TestAStreamCutOffInsideASequenceIsAbandoned(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		undo   string
	}{
		{"a title", "\x1b]0;my-title", "\x18"},
		{"a control sequence", "\x1b[38;5", "\x18"},
		{"an escape", "\x1b", "\x18"},
		{"a string's terminator", "\x1bPq#0\x1b", "\x18"},
		// Only an OSC ends on BEL; a DCS is still open after one.
		{"a DCS past a BEL", "\x1bPq#0\x07", "\x18"},
		{"a mode left on as well", "\x1b[?2004h\x1b]0;t", "\x18\x1b[?2004l"},
		{"a finished title", "\x1b]0;my-title\x1b\\", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := undoAfter(tc.output); got != tc.undo {
				t.Fatalf("undo = %q, want %q", got, tc.undo)
			}
		})
	}
}

// The stream is read as a UTF-8 terminal reads it. ESC begins a sequence after
// malformed text, so a mode a Latin-1 program turns on is not missed; and bytes
// 0x80-0x9F are text, not C1 controls, so a stray one does not invent a mode
// the terminal never entered.
func TestTheStreamIsReadAsAUTF8TerminalReadsIt(t *testing.T) {
	if got, want := undoAfter("caf\xe9\x1b[?1049h"), "\x1b[?1049l"; got != want {
		t.Fatalf("after malformed UTF-8, undo = %q, want %q", got, want)
	}
	if got := undoAfter("x\x9b?1049h"); got != "" {
		t.Fatalf("after an 8-bit CSI byte, undo = %q, want nothing", got)
	}
	if got := undoAfter("\x1b]0;\x9b?1049h\x07"); got != "" {
		t.Fatalf("after a title holding mode bytes, undo = %q, want nothing", got)
	}
}
