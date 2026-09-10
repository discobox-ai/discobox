package libkrun

import (
	"os"
	"syscall"
)

// allocatedBlocks reports the 512-byte blocks a file actually occupies, which
// is how a sparse file is told from one that was written out.
func allocatedBlocks(path string) (int64, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Blocks, true
}
