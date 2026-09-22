package sandboxuser

import (
	"strings"
	"testing"
)

func TestValidateAccountAcceptsRootAndTheGuestRange(t *testing.T) {
	for _, u := range []*User{
		nil,
		{},
		{Name: "dev", UID: ID(1000), GID: ID(1000)},
		{Name: "dev", UID: ID(60000), GID: ID(60000)},
		{Name: "discobox", UID: ID(10000), GID: ID(10000), HomeDirectory: "/home/discobox"},
		{Name: "root", UID: ID(0)},
		{UID: ID(0), GID: ID(0)},
		// A uid alone names the image's account by number; a gid it does not
		// give is read from that account inside the sandbox.
		{UID: ID(1000)},
		// Groups alone keep the image's account, so there is no uid to require.
		{AdditionalGroups: []string{"docker"}},
		{GroupName: "docker"},
		{GID: ID(2000)},
	} {
		if err := u.ValidateAccount(); err != nil {
			t.Errorf("ValidateAccount(%+v) = %v, want nil", u, err)
		}
	}
}

// A name with no uid is the request that left the pool agent chowning a
// source tree to nobody, so it is refused rather than left to boot.
func TestValidateAccountRequiresTheUIDOfANamedAccount(t *testing.T) {
	for _, u := range []*User{
		{Name: "ada"},
		{Name: "ada", GID: ID(1000)},
		{Name: "ada", HomeDirectory: "/Users/ada"},
		{HomeDirectory: "/Users/ada"},
	} {
		err := u.ValidateAccount()
		if err == nil || !strings.Contains(err.Error(), "uid") {
			t.Errorf("ValidateAccount(%+v) = %v, want the missing uid named", u, err)
		}
	}
}

// Out of range is an error, not a clamp: a macOS client's own 501/20 must not
// quietly become some other account.
func TestValidateAccountRefusesIDsOutsideTheGuestRange(t *testing.T) {
	for _, tc := range []struct {
		user  *User
		field string
	}{
		{&User{Name: "ada", UID: ID(501), GID: ID(1000)}, "uid 501"},
		{&User{Name: "ada", UID: ID(1000), GID: ID(20)}, "gid 20"},
		{&User{Name: "ada", UID: ID(1000), GID: ID(100)}, "gid 100"},
		{&User{Name: "ada", UID: ID(60001), GID: ID(1000)}, "uid 60001"},
		{&User{Name: "ada", UID: ID(-1)}, "uid -1"},
		{&User{GID: ID(20)}, "gid 20"},
	} {
		err := tc.user.ValidateAccount()
		if err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("ValidateAccount(%+v) = %v, want %s refused", tc.user, err, tc.field)
		}
	}
}

func TestValidateAccountKeepsTheInLayerCheck(t *testing.T) {
	u := &User{Name: "ada", UID: ID(1000), GID: ID(1000), GroupName: "docker"}
	if err := u.ValidateAccount(); err == nil {
		t.Fatal("ValidateAccount accepted both a gid and a group name")
	}
}
