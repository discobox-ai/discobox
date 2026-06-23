//go:build windows

package lspclient

import (
	"os"
	"os/exec"
)

func configureCommandForCleanup(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		return terminateCommand(cmd)
	}
}

func terminateCommand(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Kill()
}
