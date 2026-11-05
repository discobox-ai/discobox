package copyarchive

import (
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fsetattrlist applies nanosecond timestamps to the pinned object. futimes
// only carries microseconds; applying times by path would reintroduce a race.
func setTimes(f *os.File, a, m time.Time) error {
	attrs := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_MODTIME | unix.ATTR_CMN_ACCTIME}
	// Common attributes are packed in bitmap order: modification, then access.
	times := [2]unix.Timespec{unix.NsecToTimespec(m.UnixNano()), unix.NsecToTimespec(a.UnixNano())}
	_, _, errno := unix.Syscall6(unix.SYS_FSETATTRLIST, f.Fd(), uintptr(unsafe.Pointer(&attrs)), uintptr(unsafe.Pointer(&times)), unsafe.Sizeof(times), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
