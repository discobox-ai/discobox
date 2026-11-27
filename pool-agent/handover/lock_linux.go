package handover

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// A lock covers the whole file: start 0, length 0 is to its end, whatever it
// grows to.
func wholeFile(kind int16) *unix.Flock_t {
	return &unix.Flock_t{Type: kind, Whence: 0, Start: 0, Len: 0}
}

// tryLock takes file's lock, alone (a write lock) or shared (a read lock), if
// nobody holds it in a way that excludes that, and reports whether it did.
func tryLock(file *os.File, exclusive bool) (bool, error) {
	kind := int16(unix.F_RDLCK)
	if exclusive {
		kind = unix.F_WRLCK
	}
	for {
		err := unix.FcntlFlock(file.Fd(), unix.F_OFD_SETLK, wholeFile(kind))
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, unix.EAGAIN), errors.Is(err, unix.EACCES):
			return false, nil
		case errors.Is(err, unix.EINTR):
			continue
		default:
			return false, fmt.Errorf("lock %s: %w", file.Name(), err)
		}
	}
}

// openPending reports whether a lock file's open failed only for now: never
// on Linux, where a removed file's path is free at once.
func openPending(error) bool { return false }

// settle is what a lock does with its file once held: an open file
// description lock lasts only as long as the file is open, so it stays open.
func settle(*os.File) {}

// unlock lets go of file's lock; closing it would too.
func unlock(file *os.File) {
	_ = unix.FcntlFlock(file.Fd(), unix.F_OFD_SETLK, wholeFile(unix.F_UNLCK))
}

// lockHeld reports whether a lock that excludes a write lock is held on file
// by another open file description, without taking one: F_OFD_GETLK names
// the conflicting lock, or none.
func lockHeld(file *os.File) (bool, error) {
	probe := wholeFile(unix.F_WRLCK)
	if err := unix.FcntlFlock(file.Fd(), unix.F_OFD_GETLK, probe); err != nil {
		return false, fmt.Errorf("probe the lock %s: %w", file.Name(), err)
	}
	return probe.Type != unix.F_UNLCK, nil
}
