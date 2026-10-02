//go:build windows

package execs

import (
	"errors"
	"strings"
	"syscall"
)

// agentSysProcAttr gives no credential: a Windows sandbox has one account, the
// agent runs as it, and an exec inherits it. Running as another would take a
// logon token, and runuser.Resolve has already refused a request naming anyone
// else, a uid or a group set (ADR 0145 §5).
func agentSysProcAttr(*User) (*syscall.SysProcAttr, error) {
	return nil, nil
}

func AgentSysProcAttr(user *User) (*syscall.SysProcAttr, error) {
	return agentSysProcAttr(user)
}

// userEnvDefaults builds the environment that describes who a process is, in
// Windows's own variables. Like the POSIX one it reads the resolved identity
// rather than looking it up again.
func userEnvDefaults(user *User) (map[string]string, error) {
	if user == nil {
		return nil, nil
	}
	out := map[string]string{}
	if name := strings.TrimSpace(user.Name); name != "" {
		out["USERNAME"] = name
	}
	if home := strings.TrimSpace(user.HomeDirectory); home != "" {
		out["USERPROFILE"] = home
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
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
