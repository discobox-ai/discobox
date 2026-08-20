package termpane

import (
	"strings"
	"testing"
)

// Bytes that never passed through a line discipline carry a bare LF, and a VT
// emulator reads LF as "down one row, same column" — a staircase. On a PTY the
// kernel's ONLCR supplies the carriage return before the bytes are ever read;
// a pipe has no line discipline to supply it, so a pane onto one applies it.
func TestReadOnlyPaneDoesNotStaircasePipeOutput(t *testing.T) {
	m, stream, cmd := attach(t, 40, 6, WithReadOnly())

	stream.send("Pulling from library/busybox\nDownload complete\nPull complete\n")
	pump(t, m, cmd, "Pull complete")

	want := "Pulling from library/busybox\nDownload complete\nPull complete"
	if got := strings.TrimRight(screen(m), "\n"); got != want {
		t.Errorf("screen:\n%s\n\nwant:\n%s", got, want)
	}
}

// A far end that already emits CRLF must not gain a second carriage return.
func TestReadOnlyPaneLeavesCRLFAlone(t *testing.T) {
	m, stream, cmd := attach(t, 40, 6, WithReadOnly())

	stream.send("first\r\nsecond\r\n")
	pump(t, m, cmd, "second")

	want := "first\nsecond"
	if got := strings.TrimRight(screen(m), "\n"); got != want {
		t.Errorf("screen:\n%s\n\nwant:\n%s", got, want)
	}
}

// A lone CR still returns to column 0 without advancing, which is how a
// progress line overwrites itself — and docker prints a great many of those.
func TestReadOnlyPaneKeepsCarriageReturnOverwrite(t *testing.T) {
	m, stream, cmd := attach(t, 40, 6, WithReadOnly())

	stream.send("50%\rdone\n")
	pump(t, m, cmd, "done")

	if got := strings.TrimRight(screen(m), "\n"); got != "done" {
		t.Errorf("screen = %q, want %q", got, "done")
	}
}

// A writable pane is left as it was: its far end is a PTY, whose line
// discipline has already turned every LF into CRLF, and the mode this sets
// also changes what the Return key sends.
func TestWritablePaneLeavesLineFeedsAlone(t *testing.T) {
	m, stream, cmd := attach(t, 40, 6)

	stream.send("ab\ncd")
	pump(t, m, cmd, "cd")

	// "cd" lands in column 2, where "ab" left the cursor. That is correct VT
	// behavior, and on a real PTY it never arises because the kernel supplies
	// the carriage return.
	if got := screen(m); !strings.Contains(got, "ab\n  cd") {
		t.Errorf("screen:\n%s\n\nwant the untranslated LF to leave the cursor in column 2", got)
	}
}
