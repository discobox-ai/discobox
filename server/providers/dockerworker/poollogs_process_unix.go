//go:build !windows

package dockerworker

import (
	"os/exec"
	"syscall"
)

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup stops the log command and everything it started.
//
// The negative PID is the point: the group is what a shell's children are in,
// and a `sh -c 'journalctl ...'` whose shell alone is killed leaves the
// journalctl running with this stream's write end still open — which would
// then keep the reaping wait from ever finishing.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		// The group is gone or was never made; the process itself may still be
		// there.
		_ = cmd.Process.Kill()
	}
}
