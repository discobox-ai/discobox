//go:build !linux

package daemon

import (
	"os"
	"syscall"
)

func agentSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

func signalProcessGroup(process *os.Process, sig syscall.Signal) error {
	if process == nil {
		return nil
	}
	return process.Signal(sig)
}
