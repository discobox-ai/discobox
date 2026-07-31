//go:build !windows

package server

import (
	"errors"
	"os"
	"syscall"
)

// lockFileNB takes an exclusive advisory lock without blocking. flock is tied
// to the open file description, so the kernel drops it when the process exits
// however it exits.
func lockFileNB(file *os.File) error {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errLockBusy
	}
	return err
}

func unlockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
