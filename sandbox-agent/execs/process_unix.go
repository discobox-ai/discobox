//go:build !windows

package execs

import (
	"strings"
	"syscall"
)

func agentSysProcAttr(user *User) (*syscall.SysProcAttr, error) {
	attr := &syscall.SysProcAttr{Setsid: true}
	credential, ok, err := userCredential(user)
	if err != nil {
		return nil, err
	}
	if ok {
		attr.Credential = credential
	}
	return attr, nil
}

func AgentSysProcAttr(user *User) (*syscall.SysProcAttr, error) {
	return agentSysProcAttr(user)
}

// userEnvDefaults builds the environment that describes who a process is. It
// reads the resolved identity rather than looking it up again: Resolve was
// asked for Name and Home, so they are present or the exec never got here.
func userEnvDefaults(user *User) (map[string]string, error) {
	if user == nil {
		return nil, nil
	}
	out := map[string]string{}
	if name := strings.TrimSpace(user.Name); name != "" {
		out["USER"] = name
		out["LOGNAME"] = name
	}
	if home := strings.TrimSpace(user.HomeDirectory); home != "" {
		out["HOME"] = home
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func UserEnvDefaults(user *User) (map[string]string, error) {
	return userEnvDefaults(user)
}

// killGroup ends a process and everything it started. A one-shot runs in its
// own session (agentSysProcAttr sets Setsid), so the session's process group is
// what must be signaled: killing the child alone leaves its helpers holding
// the pipes this process is reading, and a wait does not return while anything
// can still write to them.
func killGroup(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}
