//go:build unix && !linux

package lspclient

import (
	"os"
	"os/exec"
	"syscall"
)

func configureCommandForCleanup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return terminateCommand(cmd)
	}
}

func terminateCommand(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	// Negative PID targets the process group created for the LSP hook.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	return nil
}
