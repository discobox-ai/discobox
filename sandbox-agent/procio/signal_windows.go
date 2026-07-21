//go:build windows

package procio

import (
	"os"
	"os/exec"
)

func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// signalProcessGroup has no signals to map on Windows; every request is a kill.
func signalProcessGroup(cmd *exec.Cmd, _ string) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// exitCodeFromState reports the process's exit status. Windows has no signal
// exit convention to translate, so this is the raw code.
func exitCodeFromState(state *os.ProcessState) int64 {
	return int64(state.ExitCode())
}
