//go:build !windows

package dockerworker

import (
	"os/exec"
	"syscall"
)

// processGroup ties a log command's descendants to it so one stop reaches them
// all. On unix that is a process group, which the fork itself creates.
type processGroup struct{ cmd *exec.Cmd }

func newProcessGroup(cmd *exec.Cmd) *processGroup {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &processGroup{cmd: cmd}
}

// adopt has nothing to do: Setpgid made the group as the child was forked, so
// there is no window in which a grandchild could be born outside it.
func (g *processGroup) adopt() {}

// kill stops the log command and everything it started.
//
// The negative PID is the point: the group is what a shell's children are in,
// and a `sh -c 'journalctl ...'` whose shell alone is killed leaves the
// journalctl running with this stream's write end still open — which would
// then keep the reaping wait from ever finishing.
func (g *processGroup) kill() {
	if g.cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-g.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		// The group is gone or was never made; the process itself may still be
		// there.
		_ = g.cmd.Process.Kill()
	}
}
