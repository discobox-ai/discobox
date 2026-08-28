//go:build linux

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
	return filesystemUsageFromBlocks(uint64(stat.Bsize), stat.Blocks, stat.Bfree, stat.Bavail), true
}
