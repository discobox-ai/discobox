//go:build !linux || !amd64

package libkrun

import (
	"errors"
	"syscall"
)

func validateHostPlatform() error {
	return errors.New("libkrun VMs require x86-64 Linux")
}

func detachedSysProcAttr() *syscall.SysProcAttr {
	return nil
}

func signalProcess(int, syscall.Signal) error {
	return errors.New("local libkrun VMs require Linux")
}

func lockHeld(string) bool {
	return false
}
