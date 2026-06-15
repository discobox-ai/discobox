//go:build linux

package workeragent

import "syscall"

func availableStorageBytes(path string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0
	}
	return int64(stat.Bavail) * stat.Bsize
}
