package cli

import (
	"os/user"
	"testing"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

func TestParseRunUserIdentityNonRootUnixUser(t *testing.T) {
	identity, ok, err := parseRunUserIdentity(&user.User{Username: "darren", Uid: "1000", Gid: "1000"})
	if err != nil {
		t.Fatalf("parseRunUserIdentity: %v", err)
	}
	if !ok || identity.Name != "darren" || identity.UID != 1000 || identity.GID != 1000 || !identity.IDsUsable {
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
	identity, ok, err := parseRunUserIdentity(&user.User{Username: "darren", Uid: "S-1-5-21", Gid: "S-1-5-32"})
	if err != nil {
		t.Fatalf("parseRunUserIdentity: %v", err)
	}
	if !ok || identity.Name != "darren" || identity.IDsUsable {
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

func TestRunUserIdentitySetsSandboxCreateUserFields(t *testing.T) {
	body := &apimodel.CreateSandboxBody{Name: "run"}
	runUserIdentity{Name: "darren", UID: 1000, GID: 1001, IDsUsable: true}.setCreateSandboxUser(body)

	if body.UserName.Value != "darren" || body.UserUid.Value != 1000 || body.UserGid.Value != 1001 {
		t.Fatalf("body user fields = name %q uid %d gid %d", body.UserName.Value, body.UserUid.Value, body.UserGid.Value)
	}
	if body.UserUid == (apiclientgen.OptInt64{}) || body.UserGid == (apiclientgen.OptInt64{}) {
		t.Fatalf("uid/gid options were not set: %#v", body)
	}
}

func TestRunUserIdentitySetsUsernameWithoutIDs(t *testing.T) {
	body := &apimodel.CreateSandboxBody{Name: "run"}
	runUserIdentity{Name: "darren"}.setCreateSandboxUser(body)

	if body.UserName.Value != "darren" {
		t.Fatalf("body username = %q, want darren", body.UserName.Value)
	}
	if body.UserUid.Set || body.UserGid.Set {
		t.Fatalf("uid/gid options were set unexpectedly: %#v", body)
	}
}
