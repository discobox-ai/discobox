//go:build !linux && (darwin || freebsd || netbsd || openbsd || dragonfly || solaris)

package poolagent

import "syscall"

func availableStorageBytes(path string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0
	}
	return int64(stat.Bavail) * int64(stat.Bsize)
}
