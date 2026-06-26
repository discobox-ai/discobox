//go:build windows

package execs

import (
	"os/exec"
	"syscall"
)

func agentSysProcAttr() *syscall.SysProcAttr {
	return nil
}

func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func signalProcess(cmd *exec.Cmd, _ string) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
