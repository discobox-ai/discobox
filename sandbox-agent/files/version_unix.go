//go:build !windows

package files

import (
	"io/fs"
	"strconv"
	"syscall"
)

// version tags what a path holds now. A write replaces the file by a rename,
// so its inode changes with every write, and the modification time and size
// tell an edit in place apart.
func version(info fs.FileInfo) string {
	tag := strconv.FormatInt(info.ModTime().UnixNano(), 36) + "-" + strconv.FormatInt(info.Size(), 36)
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		tag += "-" + strconv.FormatUint(uint64(sys.Ino), 36)
	}
	return tag
}
