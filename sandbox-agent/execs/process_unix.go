//go:build !windows

package execs

import (
	"fmt"
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
	name, home, err := ResolveNameAndHome(user)
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
	if user.Empty() {
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
		// Look the primary group up; never assume it equals the uid. UIDs and
		// GIDs are separate namespaces, so uid==gid is a coincidence of common
		// useradd defaults rather than a rule, and guessing it silently runs
		// the process under whatever group happens to hold that number.
		found, err := osuser.LookupId(strconv.FormatInt(uid, 10))
		if err != nil {
			return nil, false, fmt.Errorf("resolve exec user uid %d primary group: %w", uid, err)
		}
		parsed, err := strconv.ParseInt(found.Gid, 10, 64)
		if err != nil {
			return nil, false, fmt.Errorf("resolve exec user uid %d gid %q: %w", uid, found.Gid, err)
		}
		gid = parsed
	}
	if uid < 0 || uid > int64(^uint32(0)) {
		return nil, false, fmt.Errorf("exec user uid %d is out of range", uid)
	}
	if gid < 0 || gid > int64(^uint32(0)) {
		return nil, false, fmt.Errorf("exec user gid %d is out of range", gid)
	}
	groups := resolveGroups(user.AdditionalGroups)
	// NoSetGroups is deliberately NOT set. With it, the child keeps whatever
	// supplementary groups the agent has -- the agent runs as root, so an exec
	// dropped to the sandbox user inherited root's groups and none of its own.
	// That silently discarded the image's declared additionalGroups (e.g.
	// "docker"), so docker-in-sandbox only worked under an `sg docker` wrapper.
	return &syscall.Credential{
		Uid:    uint32(uid),
		Gid:    uint32(gid),
		Groups: groups,
	}, true, nil
}

func int64Value(value *int64) (int64, bool) {
	if value == nil {
		return 0, false
	}
	return *value, true
}
