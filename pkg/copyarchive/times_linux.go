package copyarchive

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func setTimes(f *os.File, a, m time.Time) error {
	return unix.UtimesNanoAt(int(f.Fd()), "", []unix.Timespec{unix.NsecToTimespec(a.UnixNano()), unix.NsecToTimespec(m.UnixNano())}, unix.AT_EMPTY_PATH)
}
