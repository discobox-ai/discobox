//go:build unix && !linux

package audit

import "golang.org/x/sys/unix"

// filesystemSize is the total size of the filesystem holding path. Statfs_t's
// field types differ across these platforms, hence the conversions.
func filesystemSize(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Blocks) * int64(stat.Bsize), nil
}
