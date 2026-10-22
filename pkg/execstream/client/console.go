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

// ResetTerminal writes undo to the terminal the remote was drawing on.
//
// Nothing is written when the output is not a terminal: a pipe or a file never
// had a mode set on it, and the undo would be bytes in somebody's capture.
func (c *OSConsole) ResetTerminal(undo string) {
	if undo == "" || c.stdout == nil || !term.IsTerminal(int(c.stdout.Fd())) {
		return
	}
	_, _ = io.WriteString(c.stdout, undo)
}

func (c *OSConsole) Size() (cols, rows int, ok bool) {
	if !c.isTerminal() {
		return 0, 0, false
	}
	cols, rows, err := term.GetSize(int(c.stdin.Fd()))
	return cols, rows, err == nil && cols > 0 && rows > 0
}
