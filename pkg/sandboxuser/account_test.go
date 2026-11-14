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
		if err := u.ValidateAccount("linux"); err != nil {
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
		err := u.ValidateAccount("linux")
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
		err := tc.user.ValidateAccount("linux")
		if err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("ValidateAccount(%+v) = %v, want %s refused", tc.user, err, tc.field)
		}
	}
}

func TestValidateAccountKeepsTheInLayerCheck(t *testing.T) {
	u := &User{Name: "ada", UID: ID(1000), GID: ID(1000), GroupName: "docker"}
	if err := u.ValidateAccount("linux"); err == nil {
		t.Fatal("ValidateAccount accepted both a gid and a group name")
	}
}

// A macOS guest gives its first ordinary account 501, in group 20 (staff), so
// a macOS client's own account is one a darwin sandbox may be created with --
// and still not one a Linux sandbox may (ADR 26-11-12-060).
func TestValidateAccountTakesTheDarwinRangeOnDarwinAlone(t *testing.T) {
	for _, u := range []*User{
		{Name: "ada", UID: ID(501), GID: ID(20)},
		{Name: "ada", UID: ID(501), GID: ID(501)},
		{Name: "dev", UID: ID(1000), GID: ID(1000)},
		{Name: "dev", UID: ID(60000), GID: ID(20)},
		{Name: "root", UID: ID(0)},
		{UID: ID(501)},
	} {
		if err := u.ValidateAccount("darwin"); err != nil {
			t.Errorf("ValidateAccount(darwin, %+v) = %v, want nil", u, err)
		}
	}
	if err := (&User{Name: "ada", UID: ID(501), GID: ID(20)}).ValidateAccount("linux"); err == nil ||
		!strings.Contains(err.Error(), "uid 501") {
		t.Errorf("ValidateAccount(linux, 501/20) = %v, want uid 501 refused", err)
	}
}

func TestValidateAccountRefusesIDsOutsideTheDarwinRange(t *testing.T) {
	for _, tc := range []struct {
		user *User
		want string
	}{
		{&User{Name: "ada", UID: ID(500), GID: ID(20)}, "uid 500 is outside the ids a darwin sandbox account may use: 0 (root) or 501-60000"},
		{&User{Name: "ada", UID: ID(60001), GID: ID(20)}, "uid 60001"},
		// Only staff is admitted below the range; root's group is not
		// one the guest gives an ordinary account.
		{&User{Name: "ada", UID: ID(501), GID: ID(80)}, "gid 80 is outside the ids a darwin sandbox account may use: 20 (staff) or 501-60000"},
		{&User{UID: ID(0), GID: ID(0)}, "gid 0"},
		{&User{Name: "ada"}, "must give its uid"},
		// The ids are all macOS adds to its one account: a group set, a group
		// by name and a home are still the account's own (ADR 0145 §5).
		{&User{Name: "ada", UID: ID(501), GID: ID(20), AdditionalGroups: []string{"admin"}}, "a group set (admin)"},
		{&User{Name: "ada", UID: ID(501), GroupName: "staff"}, `a primary group ("staff")`},
		{&User{Name: "ada", UID: ID(501), GID: ID(20), HomeDirectory: "/Users/ada"}, `a home directory ("/Users/ada")`},
	} {
		err := tc.user.ValidateAccount("darwin")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ValidateAccount(darwin, %+v) = %v, want %q", tc.user, err, tc.want)
		}
	}
}

func TestAccountIDsForEachGuest(t *testing.T) {
	for goos, want := range map[string]AccountIDs{
		"linux":  {Min: 1000, Max: 60000, Group: 0, GroupName: "root"},
		"darwin": {Min: 501, Max: 60000, Group: 20, GroupName: "staff", SharedGroup: true},
	} {
		if got, ok := AccountIDsFor(goos); !ok || got != want {
			t.Errorf("AccountIDsFor(%q) = %+v, %t, want %+v", goos, got, ok, want)
		}
	}
	for _, goos := range []string{"windows", ""} {
		if _, ok := AccountIDsFor(goos); ok {
			t.Errorf("AccountIDsFor(%q) has ids, want none", goos)
		}
	}
}

// Linux's useradd gives a new account a group of its own, numbered as its uid;
// macOS puts every account it makes in staff (ADR 26-11-12-060 §1).
func TestDefaultGIDIsTheGuests(t *testing.T) {
	linux, _ := AccountIDsFor("linux")
	darwin, _ := AccountIDsFor("darwin")
	if got := linux.DefaultGID(1500); got != 1500 {
		t.Errorf("linux DefaultGID(1500) = %d, want 1500", got)
	}
	if got := darwin.DefaultGID(501); got != 20 {
		t.Errorf("darwin DefaultGID(501) = %d, want 20", got)
	}
}
