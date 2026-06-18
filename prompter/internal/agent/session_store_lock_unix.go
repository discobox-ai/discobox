//go:build linux || darwin || freebsd || netbsd || openbsd

package agent

import (
	"os"
	"syscall"
)

func lockSessionStoreFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
}

func unlockSessionStoreFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
