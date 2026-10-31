//go:build unix

package monitors

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup runs the monitor as the leader of a group of its own, and
// ends the whole group when its run is canceled: a script's children — the
// `sleep` of a loop, the `tail` of a pipeline — would otherwise outlive it
// holding its pipes, and a final event or a disable would wait on them.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}
