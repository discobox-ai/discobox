package client

import (
	"os"

	"golang.org/x/term"
)

// OSConsole is the real terminal and signal environment: raw mode and size come
// from the caller's stdin, signals from the process.
//
// stdin is the file the terminal state is read from and set on. When it is not
// a terminal — a pipe, a redirect — every method degrades to a no-op, so a
// session attached to pipes needs no special case.
type OSConsole struct {
	stdin *os.File
}

// NewOSConsole returns a Console over stdin, or nil when stdin is not a file
// and so cannot be a terminal. A nil Console disables terminal control.
func NewOSConsole(stdin any) *OSConsole {
	file, ok := stdin.(*os.File)
	if !ok {
		return nil
	}
	return &OSConsole{stdin: file}
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

func (c *OSConsole) Size() (cols, rows int, ok bool) {
	if !c.isTerminal() {
		return 0, 0, false
	}
	cols, rows, err := term.GetSize(int(c.stdin.Fd()))
	return cols, rows, err == nil && cols > 0 && rows > 0
}
