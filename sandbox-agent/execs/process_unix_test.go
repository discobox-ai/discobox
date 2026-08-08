//go:build !windows

package execs

import (
	"os"
	osuser "os/user"
	"strconv"
	"testing"

	"github.com/obot-platform/discobox/sandbox-agent/runuser"
)

// A uid with no account cannot have its primary group resolved, and the old
// behaviour -- defaulting the gid to the uid -- silently ran the process under
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

func TestUserEnvDefaultsResolveHomeForNamedUser(t *testing.T) {
	current, err := osuser.Current()
	if err != nil {
		t.Skipf("current user unavailable: %v", err)
	}
	if current.Username == "" || current.HomeDir == "" {
		t.Skipf("current user lacks username or home: %#v", current)
	}

	env, err := userEnvDefaults(&User{Name: current.Username})
	if err != nil {
		t.Fatalf("user env defaults: %v", err)
	}
	if env["USER"] == "" || env["LOGNAME"] == "" || env["HOME"] != current.HomeDir {
		t.Fatalf("env = %#v, want current user home", env)
	}
}

func TestUserEnvDefaultsResolveHomeForNumericUser(t *testing.T) {
	current, err := osuser.Current()
	if err != nil {
		t.Skipf("current user unavailable: %v", err)
	}
	uid, err := strconv.ParseInt(current.Uid, 10, 64)
	if err != nil || current.HomeDir == "" {
		t.Skipf("current user lacks numeric uid or home: %#v", current)
	}

	env, err := userEnvDefaults(&User{UID: &uid})
	if err != nil {
		t.Fatalf("user env defaults: %v", err)
	}
	if env["HOME"] != current.HomeDir {
		t.Fatalf("env = %#v, want current user home", env)
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
	got := resolveGroups([]string{"docker", "997", "no-such-group", " ", "video"})
	if len(got) != 2 || got[0] != 997 || got[1] != 44 {
		t.Fatalf("groups = %v, want [997 44]: docker by name, 997 the same gid again, unknown and blank dropped", got)
	}
}
