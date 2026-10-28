package rawsecrets

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// onMemory refuses a directory on any filesystem but tmpfs or ramfs.
func onMemory(dir string) error {
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		return err
	}
	switch int64(fs.Type) {
	case unix.TMPFS_MAGIC, unix.RAMFS_MAGIC:
		return nil
	}
	return fmt.Errorf("it is not on a memory filesystem (type %#x)", fs.Type)
}
