//go:build windows

package server

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockFileNB takes an exclusive lock on the first byte without blocking.
// Windows releases file locks when the handle closes, which the kernel does on
// process exit, so this matches the unix flock lifetime.
func lockFileNB(file *os.File) error {
	var overlapped windows.Overlapped
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &overlapped,
	)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errLockBusy
	}
	return err
}

func unlockFile(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}
