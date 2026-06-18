//go:build windows

package agent

import (
	"os"
	"syscall"
	"unsafe"
)

const lockfileExclusiveLock = 0x00000002

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx       = kernel32.NewProc("LockFileEx")
	procUnlockFileEx     = kernel32.NewProc("UnlockFileEx")
	errWindowsLockFailed = syscall.EINVAL
)

func lockSessionStoreFile(file *os.File) error {
	var overlapped syscall.Overlapped
	ret, _, err := procLockFileEx.Call(
		file.Fd(),
		uintptr(lockfileExclusiveLock),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if ret != 0 {
		return nil
	}
	if err != syscall.Errno(0) {
		return err
	}
	return errWindowsLockFailed
}

func unlockSessionStoreFile(file *os.File) error {
	var overlapped syscall.Overlapped
	ret, _, err := procUnlockFileEx.Call(
		file.Fd(),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if ret != 0 {
		return nil
	}
	if err != syscall.Errno(0) {
		return err
	}
	return errWindowsLockFailed
}
