//go:build !windows

package execs

import (
	"os/exec"
	"strings"
	"syscall"
)

func agentSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

func signalProcess(cmd *exec.Cmd, name string) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	trimmed := strings.TrimSpace(strings.ToUpper(name))
	trimmed = strings.TrimPrefix(trimmed, "SIG")
	switch trimmed {
	case "INT":
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
	case "TERM":
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	case "KILL":
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	case "HUP":
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
	case "QUIT":
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGQUIT)
	default:
		return nil
	}
}
