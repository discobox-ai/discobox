//go:build windows

package processhelper

import (
	"os"
	"os/exec"
)

func configureHelperCommand(*exec.Cmd) {}

func configureChildCommand(*exec.Cmd) {}

func terminateProcess(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	// Closing child stdin is the portable graceful signal on Windows for stdio
	// children. The timeout path force-kills if the child ignores EOF.
	return nil
}

func killProcess(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	return process.Kill()
}
