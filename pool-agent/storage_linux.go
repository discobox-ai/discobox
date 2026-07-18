//go:build linux

package poolagent

import (
	"math"
	"syscall"
)

func availableStorageBytes(path string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0
	}
	if stat.Bsize <= 0 {
		return 0
	}
	blockSize := uint64(stat.Bsize)
	if stat.Bavail > uint64(math.MaxInt64)/blockSize {
		return math.MaxInt64
	}

	return int64(stat.Bavail * blockSize)
}
