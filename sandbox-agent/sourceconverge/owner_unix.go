//go:build !windows

package sourceconverge

import (
	"io/fs"
	"os"
	"syscall"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// fileOwner is the uid and gid that own a file.
func fileOwner(info fs.FileInfo) (uid, gid int64, ok bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int64(stat.Uid), int64(stat.Gid), true
}

// chownTo gives one file the checkout owner's ids.
func chownTo(path string, owner *execs.User) error {
	if owner.UID == nil || owner.GID == nil {
		return nil
	}
	return os.Lchown(path, int(*owner.UID), int(*owner.GID))
}
