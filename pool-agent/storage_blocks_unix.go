//go:build unix

package poolagent

import (
	"os"
	"syscall"
)

// statBlocks reports the 512-byte blocks the filesystem allocated to a file.
// The unit is fixed by the stat(2) ABI and is unrelated to the filesystem's
// block size, which is why it is not read from statfs.
func statBlocks(info os.FileInfo) (int64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Blocks * 512, true
}
