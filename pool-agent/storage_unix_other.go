//go:build !linux && (darwin || freebsd || netbsd || openbsd || dragonfly || solaris)

package poolagent

import "syscall"

// statfsField widens one statfs(2) field to the unsigned count
// filesystemUsageFromBlocks works in. The BSDs disagree on the signedness and
// width of every one of these fields — and on darwin some are already uint64,
// where a written-out conversion is dead code the linter rejects — so the
// widening goes through a type parameter instead of being spelled per field.
func statfsField[T int32 | uint32 | int64 | uint64](v T) uint64 { return uint64(v) }

func filesystemUsage(path string) (FilesystemUsage, bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return FilesystemUsage{}, false
	}
	if stat.Bsize <= 0 {
		return FilesystemUsage{}, false
	}
	return filesystemUsageFromBlocks(
		statfsField(stat.Bsize),
		statfsField(stat.Blocks),
		statfsField(stat.Bfree),
		statfsField(stat.Bavail),
	), true
}
