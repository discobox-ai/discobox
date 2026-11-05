//go:build linux || darwin

package copyarchive

import (
	"os"

	"golang.org/x/sys/unix"
)

func readFlags() int     { return os.O_RDONLY | unix.O_NONBLOCK }
func metadataFlags() int { return os.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK }
