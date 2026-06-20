//go:build linux

package daemon

import (
	"os"
	"syscall"
)

func agentSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}

func signalProcessGroup(process *os.Process, sig syscall.Signal) error {
	if process == nil {
		return nil
	}
	if err := syscall.Kill(-process.Pid, sig); err != nil {
		return process.Signal(sig)
	}
	return nil
}
