//go:build darwin

package procio

import "golang.org/x/sys/unix"

// termiosGetRequest is the ioctl that reads a terminal's attributes. See the
// Linux file for why this is per-kernel.
const termiosGetRequest = unix.TIOCGETA
