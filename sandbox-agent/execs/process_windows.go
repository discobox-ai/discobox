//go:build windows

package execs

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func agentSysProcAttr(user *User) (*syscall.SysProcAttr, error) {
	if !emptyUser(user) {
		return nil, fmt.Errorf("exec user is not supported on windows")
	}
	return nil, nil
}

func AgentSysProcAttr(user *User) (*syscall.SysProcAttr, error) {
	return agentSysProcAttr(user)
}

func userEnvDefaults(user *User) (map[string]string, error) {
	if !emptyUser(user) {
		return nil, fmt.Errorf("exec user is not supported on windows")
	}
	return nil, nil
}

func UserEnvDefaults(user *User) (map[string]string, error) {
	return userEnvDefaults(user)
}

func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func signalProcess(cmd *exec.Cmd, _ string) error {
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
