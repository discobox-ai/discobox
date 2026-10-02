//go:build unix

package sandboxruntime

import (
	"os"
	"syscall"
)

// fileOwner is the uid and gid that own info's file, or -1 for both when the
// platform does not say.
func fileOwner(info os.FileInfo) (uid, gid int) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, -1
	}
	return int(stat.Uid), int(stat.Gid)
}
