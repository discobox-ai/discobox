//go:build unix

package poolagent

import (
	"fmt"
	"os"
	"syscall"
)

// checkBootstrapAccess refuses a bootstrap file another user owns or may
// read or write: its owner could rewrite it, and choose the control plane
// this agent registers with, and any reader has its token.
func checkBootstrapAccess(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("pool bootstrap %s: no owner to check", path)
	}
	if uid := os.Geteuid(); int(stat.Uid) != uid {
		return fmt.Errorf("pool bootstrap %s is owned by uid %d, not this agent's %d", path, stat.Uid, uid)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("pool bootstrap %s has mode %04o; it holds the bootstrap token, so it must be its owner's alone (0600)", path, perm)
	}
	return nil
}
