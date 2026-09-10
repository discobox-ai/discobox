package client

import (
	"io"
	"os"

	"golang.org/x/term"
)

// OSConsole is the real terminal and signal environment: raw mode and size come
// from the caller's stdin, the display state that is put back on the way out
// from the caller's stdout, signals from the process.
//
// stdin is the file the terminal state is read from and set on, stdout the file
// the remote's output — and so every display mode it turned on — was written
// to. When either is not a terminal — a pipe, a redirect — the methods over it
// degrade to no-ops, so a session attached to pipes needs no special case.
type OSConsole struct {
	stdin  *os.File
	stdout *os.File
}

// NewOSConsole returns a Console over the caller's stdin and stdout, or nil when
// stdin is not a file and so cannot be a terminal. A nil Console disables
// terminal control. A stdout that is not a file is no obstacle: the session
// still owns the terminal's mode, it just has nowhere to write the reset.
func NewOSConsole(stdin, stdout any) *OSConsole {
	file, ok := stdin.(*os.File)
	if !ok {
		return nil
	}
	out, _ := stdout.(*os.File)
	return &OSConsole{stdin: file, stdout: out}
}

func (c *OSConsole) isTerminal() bool {
	return c.stdin != nil && term.IsTerminal(int(c.stdin.Fd()))
}

// MakeRaw puts the terminal in raw mode so Ctrl-C and Ctrl-Z reach the remote
// as bytes rather than becoming signals here.
func (c *OSConsole) MakeRaw() (func(), bool, error) {
	if !c.isTerminal() {
		return func() {}, false, nil
	}
	fd := int(c.stdin.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, false, err
	}
	return func() { _ = term.Restore(fd, state) }, true, nil
}

// ResetTerminal writes terminalReset to the terminal the remote was drawing on.
//
// Nothing is written when the output is not a terminal: a pipe or a file never
// had a mode set on it, and the reset would be bytes in somebody's capture.
func (c *OSConsole) ResetTerminal() {
	if c.stdout == nil || !term.IsTerminal(int(c.stdout.Fd())) {
		return
	}
	_, _ = io.WriteString(c.stdout, terminalReset)
}

// terminalReset turns off the display modes a program turns on for itself and
// is expected to turn off on its way out. A program on the far end of an attach
// never gets to: the attach ends where it stands — detached, disconnected, or
// killed — and every mode it set is left on a terminal it can no longer reach.
// That is the terminal that answers a click with escape bytes, prints a paste
// as gibberish, or holds a dead program's last frame in front of a live shell.
//
// Nothing here clears the screen or the scrollback. RIS ("\x1bc") would take
// the caller's own history along with the remote's mess; this is only the modes
// the remote could have turned on, which is what tmux and screen put back when
// a client detaches.
const terminalReset = "" +
	// Leave the alternate screen first, so everything after it lands on the
	// screen the caller keeps.
	"\x1b[?1049l" +
	// Mouse and focus reporting, every way of asking for it — X10 and
	// highlight tracking included, legacy but still settable — with the
	// extended encodings that carry a position. Left on, the terminal types
	// escape bytes at the shell every time the pointer moves.
	"\x1b[?9l\x1b[?1000l\x1b[?1001l\x1b[?1002l\x1b[?1003l\x1b[?1004l" +
	"\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1016l" +
	// Bracketed paste, which otherwise wraps the next paste in escapes that
	// the shell prints rather than the terminal eats.
	"\x1b[?2004l" +
	// The keyboard protocols that make ordinary keys arrive as something
	// else: the kitty stack the program pushed and never popped, xterm's
	// modifyOtherKeys, and application cursor and keypad mode — the one that
	// leaves the arrow keys sending what readline does not know.
	"\x1b[<u\x1b[=0;1u\x1b[>4;0m\x1b[?1l\x1b>" +
	// Synchronized output, which holds the screen frozen on the frame the
	// program was mid-way through drawing.
	"\x1b[?2026l" +
	// The screen itself: the cursor visible and back to its default shape,
	// the scrolling region the whole screen, wrap on, insert mode off, no
	// leftover colors, and the ASCII character set — a program cut off in
	// line-drawing mode leaves a shell that writes in box corners.
	"\x1b[?25h\x1b[0 q\x1b[r\x1b[?7h\x1b[4l\x1b[m\x1b(B\x0f"

func (c *OSConsole) Size() (cols, rows int, ok bool) {
	if !c.isTerminal() {
		return 0, 0, false
	}
	cols, rows, err := term.GetSize(int(c.stdin.Fd()))
	return cols, rows, err == nil && cols > 0 && rows > 0
}
