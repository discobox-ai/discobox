package dockerworker

import "os/exec"

// Windows has no process group to put a child in that Kill would then reach:
// killing a process leaves its children running. The backends that stream a
// log command here are the local Docker driver, whose journalctl does not
// exist on Windows, and the exec driver, whose command is the operator's own —
// so a Windows exec backend must not leave a grandchild holding its output.
func configureProcessGroup(*exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
