//go:build linux

package procio

import "golang.org/x/sys/unix"

// termiosGetRequest is the ioctl that reads a terminal's attributes. The name
// differs by kernel — Linux spells it TCGETS, the BSDs TIOCGETA — so it is the
// one part of reading them that cannot be written once.
const termiosGetRequest = unix.TCGETS
