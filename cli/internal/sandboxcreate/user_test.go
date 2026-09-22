package sandboxcreate

import (
	"os/user"
	"runtime"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/sandboxuser"
)

func TestParseRunUserIdentityNonRootUnixUser(t *testing.T) {
	identity, ok := parseRunUserIdentity(&user.User{Username: "darren", Uid: "1000", Gid: "1001", HomeDir: "/home/darren"})
	want := runUserIdentity{Name: "darren", UID: 1000, GID: 1001, HomeDirectory: "/home/darren"}
	if !ok || identity != want {
		t.Fatalf("identity = %#v, ok=%t, want %#v", identity, ok, want)
	}
}

func TestParseRunUserIdentitySkipsRoot(t *testing.T) {
	identity, ok := parseRunUserIdentity(&user.User{Username: "root", Uid: "0", Gid: "0"})
	if ok || identity != (runUserIdentity{}) {
		t.Fatalf("identity = %#v, ok=%t, want skipped", identity, ok)
	}
}

// The API refuses an id outside root and the guest's account range, so the
// client replaces one here: the uid with the first account id, the gid with the
// uid. A macOS account is 501 in group 20, and 20 is dialout in the guest.
func TestParseRunUserIdentityReplacesIDsOutsideTheGuestRange(t *testing.T) {
	for _, tc := range []struct {
		name     string
		uid, gid string
		wantUID  int64
		wantGID  int64
	}{
		{"macOS", "501", "20", 1000, 1000},
		{"a system primary group", "1500", "100", 1500, 1500},
		{"an enterprise uid", "1234567", "1234567", 1000, 1000},
		{"a uid below the range in a usable group", "999", "2000", 1000, 2000},
		{"not numeric", "S-1-5-21", "S-1-5-32", 1000, 1000},
		{"the range's edges", "60000", "1000", 60000, 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity, ok := parseRunUserIdentity(&user.User{Username: "darren", Uid: tc.uid, Gid: tc.gid, HomeDir: "/Users/darren"})
			want := runUserIdentity{Name: "darren", UID: tc.wantUID, GID: tc.wantGID, HomeDirectory: "/Users/darren"}
			if !ok || identity != want {
				t.Fatalf("identity = %#v, ok=%t, want %#v", identity, ok, want)
			}
		})
	}
}

// A name no Linux account can have is replaced like an id: the API refuses an
// account without a uid, and an account boot creates needs a name.
func TestParseRunUserIdentityNamesAnUnusableAccountDiscobox(t *testing.T) {
	identity, ok := parseRunUserIdentity(&user.User{Username: "desktop\\darren", Uid: "S-1-5-21", Gid: "S-1-5-32", HomeDir: "/Users/darren"})
	want := runUserIdentity{Name: "discobox", UID: 1000, GID: 1000, HomeDirectory: "/Users/darren"}
	if !ok || identity != want {
		t.Fatalf("identity = %#v, ok=%t, want %#v", identity, ok, want)
	}
	identity, ok = parseRunUserIdentity(&user.User{Username: "desktop\\darren", Uid: "1500", Gid: "1500", HomeDir: "C:\\Users\\darren"})
	want = runUserIdentity{Name: "discobox", UID: 1500, GID: 1500}
	if !ok || identity != want {
		t.Fatalf("identity = %#v, ok=%t, want %#v", identity, ok, want)
	}
}

// Whatever the host, the identity sent is one sandbox create accepts.
func TestParseRunUserIdentityIsAlwaysAnAccountTheAPIAccepts(t *testing.T) {
	for _, u := range []user.User{
		{Username: "darren", Uid: "501", Gid: "20", HomeDir: "/Users/darren"},
		{Username: "darren", Uid: "1000", Gid: "100", HomeDir: "/home/darren"},
		{Username: "desktop\\darren", Uid: "S-1-5-21", Gid: "S-1-5-32"},
		{Username: "darren", Uid: "70000", Gid: "70000"},
	} {
		identity, ok := parseRunUserIdentity(&u)
		if !ok {
			t.Fatalf("parseRunUserIdentity(%+v) asked for nobody", u)
		}
		sent := sandboxuser.User{Name: identity.Name, UID: sandboxuser.ID(identity.UID), GID: sandboxuser.ID(identity.GID), HomeDirectory: identity.HomeDirectory}
		if err := sent.ValidateAccount(); err != nil {
			t.Fatalf("parseRunUserIdentity(%+v) = %#v, which the API refuses: %v", u, identity, err)
		}
	}
}

func TestRunUserIdentitySetsSandboxCreateUserFields(t *testing.T) {
	body := &apimodel.CreateSandboxBody{Config: apimodel.SandboxCreateConfig{Name: "run"}}
	runUserIdentity{Name: "darren", UID: 1000, GID: 1001, HomeDirectory: "/home/darren"}.setCreateSandboxUser(body)

	sandboxUser, ok := body.Config.User.Get()
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

func TestWindowsRunUserIsAUsableSandboxIdentity(t *testing.T) {
	if !validRunUnixUserName(windowsRunUser.Name) {
		t.Fatalf("windows user name %q is not a usable unix name", windowsRunUser.Name)
	}
	if windowsRunUser.UID != 1000 || windowsRunUser.GID != 1000 {
		t.Fatalf("windows identity = %#v, want uid/gid 1000", windowsRunUser)
	}
	// Absent on purpose: boot resolves the home from the account it creates or
	// finds, so naming one here would move an existing account's home instead.
	if windowsRunUser.HomeDirectory != "" {
		t.Fatalf("windows home directory = %q, want it left to the sandbox", windowsRunUser.HomeDirectory)
	}
}

func TestWindowsRunUserSetsSandboxCreateUserFields(t *testing.T) {
	body := &apimodel.CreateSandboxBody{Config: apimodel.SandboxCreateConfig{Name: "run"}}
	windowsRunUser.setCreateSandboxUser(body)

	sandboxUser, ok := body.Config.User.Get()
	if !ok {
		t.Fatal("sandbox user was not set")
	}
	if sandboxUser.Name.Value != "discobox" || sandboxUser.UID.Value != 1000 || sandboxUser.Gid.Value != 1000 {
		t.Fatalf("body user = name %q uid %d gid %d, want discobox/1000/1000", sandboxUser.Name.Value, sandboxUser.UID.Value, sandboxUser.Gid.Value)
	}
	if !sandboxUser.UID.Set || !sandboxUser.Gid.Set {
		t.Fatalf("uid/gid options were not set: %#v", sandboxUser)
	}
	if sandboxUser.HomeDirectory.Set {
		t.Fatalf("home directory was sent as %q, want absent", sandboxUser.HomeDirectory.Value)
	}
}

func TestResolveRunUserIdentityOnWindowsIsFixed(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only path")
	}
	identity, ok, err := resolveRunUserIdentity()
	if err != nil {
		t.Fatalf("resolveRunUserIdentity: %v", err)
	}
	if !ok || identity != windowsRunUser {
		t.Fatalf("identity = %#v, ok=%t, want %#v", identity, ok, windowsRunUser)
	}
}
