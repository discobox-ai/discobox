//go:build !windows

package execs

import (
	"fmt"
	"os"
	"os/exec"
	osuser "os/user"
	"strconv"
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

func userEnvDefaults(user *User) (map[string]string, error) {
	name, home, err := ResolveUser(user)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if name != "" {
		out["USER"] = name
		out["LOGNAME"] = name
	}
	if home != "" {
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

func userCredential(user *User) (*syscall.Credential, bool, error) {
	if emptyUser(user) {
		return nil, false, nil
	}
	uid, uidOK := int64Value(user.UID)
	gid, gidOK := int64Value(user.GID)
	if name := strings.TrimSpace(user.Name); name != "" {
		found, err := osuser.Lookup(name)
		if err != nil {
			return nil, false, fmt.Errorf("resolve exec user %q: %w", name, err)
		}
		if !uidOK {
			parsed, err := strconv.ParseInt(found.Uid, 10, 64)
			if err != nil {
				return nil, false, fmt.Errorf("resolve exec user %q uid %q: %w", name, found.Uid, err)
			}
			uid = parsed
			uidOK = true
		}
		if !gidOK {
			parsed, err := strconv.ParseInt(found.Gid, 10, 64)
			if err != nil {
				return nil, false, fmt.Errorf("resolve exec user %q gid %q: %w", name, found.Gid, err)
			}
			gid = parsed
			gidOK = true
		}
	}
	if !uidOK {
		return nil, false, fmt.Errorf("exec user uid is required")
	}
	if !gidOK {
		gid = uid
	}
	if uid < 0 || uid > int64(^uint32(0)) {
		return nil, false, fmt.Errorf("exec user uid %d is out of range", uid)
	}
	if gid < 0 || gid > int64(^uint32(0)) {
		return nil, false, fmt.Errorf("exec user gid %d is out of range", gid)
	}
	return &syscall.Credential{
		Uid:         uint32(uid),
		Gid:         uint32(gid),
		NoSetGroups: true,
	}, true, nil
}

func int64Value(value *int64) (int64, bool) {
	if value == nil {
		return 0, false
	}
	return *value, true
}

func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

func signalProcess(cmd *exec.Cmd, name string) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	trimmed := strings.TrimSpace(strings.ToUpper(name))
	trimmed = strings.TrimPrefix(trimmed, "SIG")
	switch trimmed {
	case "INT":
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
	case "TERM":
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	case "KILL":
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	case "HUP":
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
	case "QUIT":
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGQUIT)
	default:
		return nil
	}
}

// exitCodeFromState reports the exit status a shell would report. Go's
// ExitCode returns -1 for a process killed by a signal, which loses which
// signal it was and reads as a generic failure; the shell convention of
// 128+signum keeps it, so an interrupted command exits 130 as it does locally.
func exitCodeFromState(state *os.ProcessState) int64 {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return int64(128 + status.Signal())
	}
	return int64(state.ExitCode())
}
