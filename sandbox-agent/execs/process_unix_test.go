//go:build !windows

package execs

import (
	"os"
	osuser "os/user"
	"strconv"
	"testing"

	"github.com/obot-platform/discobox/sandbox-agent/runuser"
	"github.com/obot-platform/discobox/sandboxuser"
)

// A uid with no account cannot have its primary group resolved, and the old
// behavior -- defaulting the gid to the uid -- silently ran the process under
// whatever group happened to hold that number. UIDs and GIDs are separate
// namespaces, so that is a guess, and an unresolvable uid is now an error.
func TestUserCredentialRefusesToInventAGID(t *testing.T) {
	uid := int64(1234)
	if _, _, err := userCredential(&User{UID: &uid}); err == nil {
		t.Fatal("expected an error rather than an invented gid for a uid with no account")
	}
}

// An explicit gid is taken as given: the manifest is the source of truth.
func TestUserCredentialUsesExplicitGID(t *testing.T) {
	uid, gid := int64(1234), int64(5678)
	credential, ok, err := userCredential(&User{UID: &uid, GID: &gid})
	if err != nil || !ok {
		t.Fatalf("user credential: ok=%v err=%v", ok, err)
	}
	if credential.Uid != 1234 || credential.Gid != 5678 {
		t.Fatalf("credential = %d:%d, want 1234:5678", credential.Uid, credential.Gid)
	}
}

// The process environment describes whoever the exec runs as. It is built from
// the resolved identity rather than by looking the user up again: resolution
// happens once, in runuser, and a second lookup here is how the two came to
// disagree about where home was.
func TestUserEnvDefaultsDescribeTheResolvedUser(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())

	for name, layer := range map[string]*User{
		"by name": {Name: "dev"},
		// A numeric identity gets its name and home from the same passwd entry,
		// so both spellings of the same user produce the same environment.
		"by uid": {UID: sandboxuser.ID(1000)},
	} {
		t.Run(name, func(t *testing.T) {
			resolved, err := runuser.Resolve(runuser.Layers{Manifest: layer}, sandboxuser.Complete)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			env, err := userEnvDefaults(&resolved)
			if err != nil {
				t.Fatalf("user env defaults: %v", err)
			}
			if env["USER"] != "dev" || env["LOGNAME"] != "dev" || env["HOME"] != "/home/dev" {
				t.Fatalf("env = %#v, want dev's name and home", env)
			}
		})
	}
}

// An identity that was never resolved must not quietly produce a half-built
// environment: the fields are absent because nobody filled them, not because
// the user has no name.
func TestUserEnvDefaultsOnAnEmptyUserYieldsNothing(t *testing.T) {
	env, err := userEnvDefaults(&User{UID: sandboxuser.ID(1000)})
	if err != nil {
		t.Fatalf("user env defaults: %v", err)
	}
	if len(env) != 0 {
		t.Fatalf("env = %#v, want nothing: no name or home was resolved", env)
	}
}

// The agent runs as root. With NoSetGroups the child kept root's supplementary
// groups and none of the sandbox user's, silently discarding every group the
// image declared -- which is why docker-in-sandbox needed an `sg docker`
// wrapper. Groups must be set explicitly from the manifest.
func TestCredentialSetsManifestGroups(t *testing.T) {
	uid := int64(os.Getuid())
	cred, ok, err := userCredential(&User{UID: &uid, GID: &uid, AdditionalGroups: []string{"root"}})
	if err != nil || !ok {
		t.Fatalf("userCredential: ok=%v err=%v", ok, err)
	}
	if cred.NoSetGroups {
		t.Fatal("NoSetGroups must not be set: the child would inherit the agent's groups")
	}
	if len(cred.Groups) != 1 || cred.Groups[0] != 0 {
		t.Fatalf("Groups = %v, want the resolved gid for \"root\"", cred.Groups)
	}
}

// A group the manifest names but the image never created is skipped, matching
// boot's ensureAdditionalGroups; the two must not disagree about one image.
func TestCredentialSkipsUnknownGroups(t *testing.T) {
	uid := int64(os.Getuid())
	cred, ok, err := userCredential(&User{UID: &uid, GID: &uid,
		AdditionalGroups: []string{"root", "discobox-no-such-group"}})
	if err != nil || !ok {
		t.Fatalf("userCredential: ok=%v err=%v", ok, err)
	}
	if len(cred.Groups) != 1 {
		t.Fatalf("unknown group should be skipped, got %v", cred.Groups)
	}
}

// UIDs and GIDs are separate namespaces, so a missing gid must be looked up,
// never assumed equal to the uid.
func TestCredentialResolvesPrimaryGroupRatherThanGuessing(t *testing.T) {
	uid := int64(os.Getuid())
	cred, ok, err := userCredential(&User{UID: &uid})
	if err != nil {
		t.Skipf("uid %d has no account entry here: %v", uid, err)
	}
	if !ok {
		t.Fatal("expected a credential")
	}
	self, err := osuser.LookupId(strconv.FormatInt(uid, 10))
	if err != nil {
		t.Skipf("cannot resolve uid %d: %v", uid, err)
	}
	want, err := strconv.ParseInt(self.Gid, 10, 64)
	if err != nil {
		t.Fatalf("parse gid: %v", err)
	}
	if cred.Gid != uint32(want) {
		t.Fatalf("Gid = %d, want the account's real gid %d", cred.Gid, want)
	}
}

// resolveGroups drops what it cannot resolve and collapses duplicates. Entry
// forms themselves are covered by TestLookupGroupIDAcceptsNamesAndNumbers.
func TestResolveGroupsSkipsUnknownAndDeduplicates(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	got := runuser.Groups([]string{"docker", "997", "no-such-group", " ", "video"})
	if len(got) != 2 || got[0] != 997 || got[1] != 44 {
		t.Fatalf("groups = %v, want [997 44]: docker by name, 997 the same gid again, unknown and blank dropped", got)
	}
}
