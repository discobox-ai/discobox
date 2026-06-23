package cli

import (
	"os/user"
	"testing"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

func TestParseRunUserIdentityNonRootUnixUser(t *testing.T) {
	identity, ok, err := parseRunUserIdentity(&user.User{Username: "darren", Uid: "1000", Gid: "1000", HomeDir: "/home/darren"})
	if err != nil {
		t.Fatalf("parseRunUserIdentity: %v", err)
	}
	if !ok || identity.Name != "darren" || identity.UID != 1000 || identity.GID != 1000 || identity.HomeDirectory != "/home/darren" || !identity.IDsUsable {
		t.Fatalf("identity = %#v, ok=%t", identity, ok)
	}
}

func TestParseRunUserIdentitySkipsRoot(t *testing.T) {
	identity, ok, err := parseRunUserIdentity(&user.User{Username: "root", Uid: "0", Gid: "0"})
	if err != nil {
		t.Fatalf("parseRunUserIdentity: %v", err)
	}
	if ok || identity != (runUserIdentity{}) {
		t.Fatalf("identity = %#v, ok=%t, want skipped", identity, ok)
	}
}

func TestParseRunUserIdentityUsesValidUsernameWhenIDsAreNotNumeric(t *testing.T) {
	identity, ok, err := parseRunUserIdentity(&user.User{Username: "darren", Uid: "S-1-5-21", Gid: "S-1-5-32", HomeDir: "/Users/darren"})
	if err != nil {
		t.Fatalf("parseRunUserIdentity: %v", err)
	}
	if !ok || identity.Name != "darren" || identity.HomeDirectory != "/Users/darren" || identity.IDsUsable {
		t.Fatalf("identity = %#v, ok=%t, want username only", identity, ok)
	}
}

func TestParseRunUserIdentitySkipsInvalidUsernameAndNonNumericIDs(t *testing.T) {
	identity, ok, err := parseRunUserIdentity(&user.User{Username: "desktop\\darren", Uid: "S-1-5-21", Gid: "S-1-5-32"})
	if err != nil {
		t.Fatalf("parseRunUserIdentity: %v", err)
	}
	if ok || identity != (runUserIdentity{}) {
		t.Fatalf("identity = %#v, ok=%t, want skipped", identity, ok)
	}
}

func TestParseRunUserIdentityUsesHomeDirectoryWhenUsernameIsInvalid(t *testing.T) {
	identity, ok, err := parseRunUserIdentity(&user.User{Username: "desktop\\darren", Uid: "S-1-5-21", Gid: "S-1-5-32", HomeDir: "/Users/darren"})
	if err != nil {
		t.Fatalf("parseRunUserIdentity: %v", err)
	}
	if !ok || identity.Name != "" || identity.HomeDirectory != "/Users/darren" || identity.IDsUsable {
		t.Fatalf("identity = %#v, ok=%t, want home directory only", identity, ok)
	}
}

func TestRunUserIdentitySetsSandboxCreateUserFields(t *testing.T) {
	body := &apimodel.CreateSandboxBody{Name: "run"}
	runUserIdentity{Name: "darren", UID: 1000, GID: 1001, HomeDirectory: "/home/darren", IDsUsable: true}.setCreateSandboxUser(body)

	sandboxUser, ok := body.User.Get()
	if !ok {
		t.Fatal("sandbox user was not set")
	}
	if sandboxUser.Name.Value != "darren" || sandboxUser.UID.Value != 1000 || sandboxUser.Gid.Value != 1001 || sandboxUser.HomeDirectory.Value != "/home/darren" {
		t.Fatalf("body user fields = name %q uid %d gid %d home %q", sandboxUser.Name.Value, sandboxUser.UID.Value, sandboxUser.Gid.Value, sandboxUser.HomeDirectory.Value)
	}
	if sandboxUser.UID == (apiclientgen.OptInt64{}) || sandboxUser.Gid == (apiclientgen.OptInt64{}) {
		t.Fatalf("uid/gid options were not set: %#v", body)
	}
}

func TestRunUserIdentitySetsUsernameWithoutIDs(t *testing.T) {
	body := &apimodel.CreateSandboxBody{Name: "run"}
	runUserIdentity{Name: "darren"}.setCreateSandboxUser(body)

	sandboxUser, ok := body.User.Get()
	if !ok {
		t.Fatal("sandbox user was not set")
	}
	if sandboxUser.Name.Value != "darren" {
		t.Fatalf("body username = %q, want darren", sandboxUser.Name.Value)
	}
	if sandboxUser.UID.Set || sandboxUser.Gid.Set {
		t.Fatalf("uid/gid options were set unexpectedly: %#v", body)
	}
}
