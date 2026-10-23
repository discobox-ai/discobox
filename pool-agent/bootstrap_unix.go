//go:build unix

package poolagent

import (
	"fmt"
	"os"
	"syscall"
)

// checkBootstrapOwner refuses a bootstrap file another user owns: its owner
// could rewrite it, and choose the control plane this agent registers with.
func checkBootstrapOwner(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("pool bootstrap %s: no owner to check", path)
	}
	if uid := os.Geteuid(); int(stat.Uid) != uid {
		return fmt.Errorf("pool bootstrap %s is owned by uid %d, not this agent's %d", path, stat.Uid, uid)
	}
	return nil
}
