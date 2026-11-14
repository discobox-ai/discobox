//go:build unix

package poolagent

import (
	"os"
	"syscall"
)

// statBlocks reports the 512-byte blocks the filesystem allocated to a file.
// The unit is fixed by the stat(2) ABI and is unrelated to the filesystem's
// block size, which is why it is not read from statfs.
func statBlocks(info os.FileInfo) (int64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Blocks * 512, true
}

// statDevice is the device the filesystem holding path is on, which tells two
// trees on different disks apart where the paths alone do not.
func statDevice(path string) (uint64, bool) {
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return 0, false
	}
	return uint64(stat.Dev), true //nolint:unconvert // Dev's width differs by platform.
}
