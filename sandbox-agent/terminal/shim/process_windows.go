//go:build windows

package shim

import (
	"os"
	"syscall"
)

func agentSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

func terminateProcessGroup(process *os.Process) error {
	if process == nil {
		return nil
	}
	return process.Kill()
}

func interruptSignal() os.Signal { return os.Interrupt }
func terminateSignal() os.Signal { return os.Kill }
func killSignal() os.Signal      { return os.Kill }
func hangupSignal() os.Signal    { return os.Interrupt }
func quitSignal() os.Signal      { return os.Interrupt }
