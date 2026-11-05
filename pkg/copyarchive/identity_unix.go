//go:build linux || darwin

package copyarchive

import (
	"fmt"
	"io/fs"
	"syscall"
)

// FileIdentity identifies a filesystem object, including hard-link aliases.
func FileIdentity(_ string, info fs.FileInfo) (string, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("filesystem identity unavailable")
	}
	return fmt.Sprintf("%x:%x", stat.Dev, stat.Ino), nil
}
