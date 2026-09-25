//go:build windows

package execs

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/discobox-ai/discobox/sandboxuser"
)

func agentSysProcAttr(user *User) (*syscall.SysProcAttr, error) {
	if sandboxuser.Named(user) {
		return nil, fmt.Errorf("exec user is not supported on windows")
	}
	return nil, nil
}

func AgentSysProcAttr(user *User) (*syscall.SysProcAttr, error) {
	return agentSysProcAttr(user)
}

func userEnvDefaults(user *User) (map[string]string, error) {
	if sandboxuser.Named(user) {
		return nil, fmt.Errorf("exec user is not supported on windows")
	}
	return nil, nil
}

func UserEnvDefaults(user *User) (map[string]string, error) {
	return userEnvDefaults(user)
}

// killGroup has no session to signal on Windows; the process itself is what
// there is. The sandbox runtime is Linux, and this exists so the package still
// builds for the cross-check.
func killGroup(int) error { return errors.ErrUnsupported }

// chownToUser has nothing to do: a command runs as this process's own user.
func chownToUser(string, *User) error { return nil }
