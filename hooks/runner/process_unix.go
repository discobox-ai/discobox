//go:build !windows

package runner

import (
	"os"
	"os/exec"
	"syscall"
)

func configureCommandForGroupKill(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		// Negative PID targets the process group created for the hook.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			_ = cmd.Process.Kill()
			return err
		}
		return nil
	}
}
