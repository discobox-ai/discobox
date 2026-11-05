package copyarchive

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

func readFlags() int     { return os.O_RDONLY }
func metadataFlags() int { return os.O_RDONLY }

var reopenFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

func setTimes(f *os.File, a, m time.Time) error {
	for _, stamp := range []time.Time{a, m} {
		if stamp.Nanosecond()%100 != 0 {
			return fmt.Errorf("Windows cannot preserve sub-100ns timestamps")
		}
		if !time.Unix(0, stamp.UnixNano()).Equal(stamp) {
			return fmt.Errorf("timestamp is outside the supported preservation range")
		}
	}

	// Reopen the pinned object with attribute-write access. A directory opened
	// for traversal need not have that access, and reopening by pathname races.
	handle, _, err := reopenFile.Call(f.Fd(), windows.FILE_WRITE_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_FLAG_BACKUP_SEMANTICS)
	if windows.Handle(handle) == windows.InvalidHandle {
		return os.NewSyscallError("ReOpenFile", err)
	}
	defer func() { _ = windows.CloseHandle(windows.Handle(handle)) }()
	at, mt := windows.NsecToFiletime(a.UnixNano()), windows.NsecToFiletime(m.UnixNano())
	return windows.SetFileTime(windows.Handle(handle), nil, &at, &mt)
}

func accessTime(info fs.FileInfo) time.Time {
	v, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return info.ModTime()
	}
	return time.Unix(0, v.LastAccessTime.Nanoseconds())
}
