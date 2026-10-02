package execs

import (
	"fmt"
	"os"
	"syscall"

	"github.com/discobox-ai/discobox/sandbox-agent/runuser"
	"github.com/discobox-ai/discobox/sandboxuser"
)

// The credential an exec is launched with is Linux's: a sandbox there runs
// processes by uid, gid and group set, and the agent, running as root, drops
// each exec to the identity runuser resolved. Where a sandbox has one account
// the agent already is it, and an exec inherits it (credential_darwin.go).

// userCredential turns a resolved identity into the credential the launch path
// applies. It looks nothing up.
//
// It deliberately does not repeat Resolve's name->ids and uid->gid lookups as a
// last line of defense against reaching setuid with an invented gid (ADR 0025
// §6). Such a defense fires only when an id is *absent*, so it catches a
// missing gid and is blind to a wrong one -- an id invented upstream arrives
// fully populated and passes straight through. Requiring the ids makes the
// stronger claim: Resolve was asked for a credential, so both are filled or the
// call failed, and anything else here is a broken invariant rather than
// something to go and complete (ADR 0033 §6). It also keeps this path behind
// the test fixture, which a direct os/user call could never be.
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

// chownToUser gives path to the user a command runs as, when it runs as one.
func chownToUser(path string, user *User) error {
	credential, ok, err := userCredential(user)
	if err != nil || !ok {
		return err
	}
	return os.Chown(path, int(credential.Uid), int(credential.Gid))
}
