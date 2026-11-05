package copyarchive

import (
	"io/fs"
	"syscall"
	"time"
)

func accessTime(info fs.FileInfo) time.Time {
	v, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.ModTime()
	}
	return time.Unix(v.Atimespec.Sec, v.Atimespec.Nsec)
}
