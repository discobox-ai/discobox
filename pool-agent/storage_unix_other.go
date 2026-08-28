//go:build !linux && (darwin || freebsd || netbsd || openbsd || dragonfly || solaris)

package poolagent

import "syscall"

func filesystemUsage(path string) (FilesystemUsage, bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return FilesystemUsage{}, false
	}
	if stat.Bsize <= 0 {
		return FilesystemUsage{}, false
	}
	return filesystemUsageFromBlocks(uint64(stat.Bsize), uint64(stat.Blocks), uint64(stat.Bfree), uint64(stat.Bavail)), true
}
