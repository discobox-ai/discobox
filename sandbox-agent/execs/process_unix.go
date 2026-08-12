//go:build !windows

package execs

import (
	"fmt"
	"strings"
	"syscall"

	"github.com/obot-platform/discobox/sandbox-agent/runuser"
	"github.com/obot-platform/discobox/sandboxuser"
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

// userCredential turns a resolved identity into the credential the launch path
// applies. It looks nothing up.
//
// It used to repeat Resolve's name->ids and uid->gid lookups here, as a last
// line of defense against reaching setuid with an invented gid (ADR 0025 §6).
// That defense only ever fired when an id was *absent*, so it caught a missing
// gid and was blind to a wrong one -- an id invented upstream arrives fully
// populated and passes straight through. Requiring the ids instead makes the
// stronger claim: Resolve was asked for a credential, so both are filled or the
// call failed, and anything else here is a broken invariant rather than
// something to go and complete (ADR 0032 §6). It also puts this path behind the
// test fixture, which a direct os/user call could never be.
func userCredential(user *User) (*syscall.Credential, bool, error) {
	if !sandboxuser.Named(user) {
		return nil, false, nil
	}
	if user.UID == nil || user.GID == nil {
		return nil, false, fmt.Errorf("exec user %q reached launch unresolved: uid or gid is absent", user.Name)
	}
	uid, gid := *user.UID, *user.GID
	if uid < 0 || uid > int64(^uint32(0)) {
		return nil, false, fmt.Errorf("exec user uid %d is out of range", uid)
	}
	if gid < 0 || gid > int64(^uint32(0)) {
		return nil, false, fmt.Errorf("exec user gid %d is out of range", gid)
	}
	groups := runuser.Groups(user.AdditionalGroups)
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
