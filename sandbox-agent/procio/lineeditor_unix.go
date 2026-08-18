//go:build !windows

package procio

import (
	"time"

	"golang.org/x/sys/unix"
)

// How long to wait for the line editor, and how often to look.
//
// The wait is bounded because not every program on a TTY takes the terminal:
// one that reads in canonical mode leaves ECHO on for its whole life, and
// waiting on it forever would hold up input it is perfectly ready for. The cap
// is generous next to what this actually costs — a shell was measured taking
// ~10ms, or about ten of these — because the price of being early is visible
// and the price of being late is not.
const (
	lineEditorWait  = 2 * time.Second
	lineEditorCheck = 500 * time.Microsecond
)

// WaitForLineEditor blocks until the program on the TTY has taken the terminal
// for its own line editing, or the wait runs out. It reports whether it did.
//
// This is what makes typed-in input appear once. Until a line editor preps the
// terminal, ECHO is on and the kernel echoes anything written to the PTY as it
// arrives; the editor then reads those same bytes and displays them again as
// the line it is editing, so the input lands on screen twice — once above the
// prompt and once on it. Waiting for ECHO to go means only the editor shows it.
//
// It is a poll because Linux offers nothing to wait on: a PTY in packet mode
// (TIOCPKT) is documented to report slave state changes as TIOCPKT_IOCTL, but
// that bit is BSD's and is not implemented here — verified against a real
// shell, which preps its terminal and delivers no such packet.
//
// A process with no TTY has no line discipline to wait for and returns at once.
func (p *Process) WaitForLineEditor() bool {
	if p.tty == nil {
		return true
	}
	fd := int(p.tty.Fd())
	deadline := time.Now().Add(lineEditorWait)
	for {
		// The master answers for the slave's line discipline, so the fd this
		// already holds is the one to ask.
		termios, err := unix.IoctlGetTermios(fd, unix.TCGETS)
		if err != nil {
			// The TTY went away, or is not one this can ask. Either way there
			// is nothing to wait for.
			return false
		}
		if termios.Lflag&unix.ECHO == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(lineEditorCheck)
	}
}
