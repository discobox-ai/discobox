//go:build !windows

package shim

import (
	"os"
	"syscall"
)

func agentSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

func terminateProcessGroup(process *os.Process) error {
	if process == nil || process.Pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-process.Pid, syscall.SIGTERM); err == nil {
		return nil
	}
	return process.Signal(syscall.SIGTERM)
}

func interruptSignal() os.Signal { return syscall.SIGINT }
func terminateSignal() os.Signal { return syscall.SIGTERM }
func killSignal() os.Signal      { return syscall.SIGKILL }
func hangupSignal() os.Signal    { return syscall.SIGHUP }
func quitSignal() os.Signal      { return syscall.SIGQUIT }
