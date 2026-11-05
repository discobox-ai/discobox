package copyarchive

import (
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func readFlags() int     { return os.O_RDONLY }
func metadataFlags() int { return os.O_RDONLY }

var reopenFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

func setTimes(f *os.File, a, m time.Time) error {
	defer runtime.KeepAlive(f)
	for _, stamp := range []time.Time{a, m} {
		if stamp.Nanosecond()%100 != 0 {
			return fmt.Errorf("windows cannot preserve sub-100ns timestamps")
		}
		if !time.Unix(0, stamp.UnixNano()).Equal(stamp) {
			return fmt.Errorf("timestamp is outside the supported preservation range")
		}
	}

	// Reopen the pinned object with attribute-write access, never by pathname.
	// ReOpenFile rejects directory handles, so reopen those through an empty
	// object name relative to the directory handle itself.
	info, err := f.Stat()
	if err != nil {
		return err
	}
	var handle windows.Handle
	if info.IsDir() {
		name := &windows.NTUnicodeString{}
		attrs := &windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(f.Fd()), ObjectName: name}
		attrs.Length = uint32(unsafe.Sizeof(*attrs))
		err = windows.NtCreateFile(&handle, windows.FILE_WRITE_ATTRIBUTES|windows.SYNCHRONIZE, attrs, &windows.IO_STATUS_BLOCK{}, nil, 0,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
			windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
		if err != nil {
			return os.NewSyscallError("NtCreateFile", err)
		}
	} else {
		value, _, callErr := reopenFile.Call(f.Fd(), windows.FILE_WRITE_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT)
		handle = windows.Handle(value)
		if handle == windows.InvalidHandle {
			return os.NewSyscallError("ReOpenFile", callErr)
		}
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	at, mt := windows.NsecToFiletime(a.UnixNano()), windows.NsecToFiletime(m.UnixNano())
	return windows.SetFileTime(handle, nil, &at, &mt)
}

func accessTime(info fs.FileInfo) time.Time {
	v, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return info.ModTime()
	}
	return time.Unix(0, v.LastAccessTime.Nanoseconds())
}
