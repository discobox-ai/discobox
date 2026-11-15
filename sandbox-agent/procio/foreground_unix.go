//go:build !windows

package procio

import (
	"os"

	"golang.org/x/sys/unix"
)

// ForegroundGroup is the process group in the foreground of the PTY tty is
// the master of, or zero when there is no PTY or it cannot be read (closed by
// Hangup, or a session with no foreground group). It is the group a local
// terminal's keystroke signals reach: an interactive shell runs each command
// it is given as a job in a group of its own and hands that group the
// terminal. It goes through SyscallConn rather than Fd, which would put the
// master into blocking mode under the reader.
func ForegroundGroup(tty *os.File) int {
	if tty == nil {
		return 0
	}
	conn, err := tty.SyscallConn()
	if err != nil {
		return 0
	}
	group := 0
	_ = conn.Control(func(fd uintptr) {
		if pgid, err := unix.IoctlGetInt(int(fd), unix.TIOCGPGRP); err == nil && pgid > 0 {
			group = pgid
		}
	})
	return group
}
