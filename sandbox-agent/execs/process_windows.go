//go:build windows

package execs

import (
	"fmt"
	"syscall"
)

func agentSysProcAttr(user *User) (*syscall.SysProcAttr, error) {
	if !user.Empty() {
		return nil, fmt.Errorf("exec user is not supported on windows")
	}
	return nil, nil
}

func AgentSysProcAttr(user *User) (*syscall.SysProcAttr, error) {
	return agentSysProcAttr(user)
}

func userEnvDefaults(user *User) (map[string]string, error) {
	if !user.Empty() {
		return nil, fmt.Errorf("exec user is not supported on windows")
	}
	return nil, nil
}

func UserEnvDefaults(user *User) (map[string]string, error) {
	return userEnvDefaults(user)
}
