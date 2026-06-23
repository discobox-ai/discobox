//go:build unix

package processhelper

import (
	"os"
	"os/exec"
	"syscall"
)

func configureHelperCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func configureChildCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcess(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-process.Pid, syscall.SIGTERM); err != nil {
		return process.Signal(syscall.SIGTERM)
	}
	return nil
}

func killProcess(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-process.Pid, syscall.SIGKILL); err != nil {
		return process.Kill()
	}
	return nil
}
