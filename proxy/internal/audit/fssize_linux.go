package audit

import "golang.org/x/sys/unix"

// filesystemSize is the total size of the filesystem holding path.
func filesystemSize(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Blocks) * stat.Bsize, nil
}
