//go:build linux || darwin

package sandboxcreate

import (
	"io/fs"
	"syscall"
)

func allocatedBytes(_ string, info fs.FileInfo) int64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Blocks * 512
	}
	return info.Size()
}
