package sandboxuser

import (
	"errors"
	"strings"
	"testing"
)

func TestHasPOSIXIDsOnlyOnLinux(t *testing.T) {
	for goos, want := range map[string]bool{"linux": true, "darwin": false, "windows": false} {
		if got := HasPOSIXIDs(goos); got != want {
			t.Errorf("HasPOSIXIDs(%q) = %v, want %v", goos, got, want)
		}
	}
}

func TestValidateOneAccountAcceptsTheAccountOrNobody(t *testing.T) {
	for _, u := range []*User{nil, {}, {Name: "dev"}, {Name: " dev "}} {
		if err := u.ValidateOneAccount("darwin", "dev"); err != nil {
			t.Errorf("ValidateOneAccount(%+v) = %v, want nil", u, err)
		}
	}
	// Not knowing the account, the control plane accepts any one name.
	if err := (&User{Name: "anyone"}).ValidateOneAccount("darwin", ""); err != nil {
		t.Errorf("ValidateOneAccount with no known account = %v, want nil", err)
	}
}

func TestValidateOneAccountRefusesWhatTheSandboxDoesNotHave(t *testing.T) {
	for name, tc := range map[string]struct {
		user  User
		field Fields
		says  string
	}{
		"another user":           {User{Name: "root"}, FieldName, `another account ("root")`},
		"a uid":                  {User{Name: "dev", UID: ID(501)}, FieldUID, "a uid (501)"},
		"a gid":                  {User{GID: ID(20)}, FieldGID, "a primary group (20)"},
		"a group name":           {User{GroupName: "staff"}, FieldGID, `a primary group ("staff")`},
		"a group set":            {User{AdditionalGroups: []string{"docker", "video"}}, FieldGroups, "a group set (docker, video)"},
		"another home":           {User{HomeDirectory: "/Users/other"}, FieldHome, `another home directory ("/Users/other")`},
		"its own name and a uid": {User{Name: "dev", UID: ID(1000)}, FieldUID, "a uid (1000)"},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.user.ValidateOneAccount("darwin", "dev")
			var refused *OneAccountError
			if !errors.As(err, &refused) {
				t.Fatalf("err = %v, want a OneAccountError", err)
			}
			if refused.Field != tc.field {
				t.Errorf("field = %s, want %s", refused.Field, tc.field)
			}
			// The reason is the platform's, and the message says so.
			for _, want := range []string{"darwin", `one account, "dev"`, "no POSIX ids", tc.says} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}

func TestValidateOneAccountKeepsTheInLayerCheck(t *testing.T) {
	u := &User{GID: ID(5), GroupName: "staff"}
	if err := u.ValidateOneAccount("darwin", ""); !errors.Is(err, errBothGIDAndGroupName) {
		t.Fatalf("err = %v, want the in-layer contradiction", err)
	}
}

// A sandbox create on a platform without POSIX ids takes the one-account rule,
// and the Linux account-range rule does not apply: a client's own uid is not
// something such a sandbox can be created with at all.
func TestValidateAccountOffLinuxIsTheOneAccountRule(t *testing.T) {
	if err := (&User{Name: "dev"}).ValidateAccount("darwin"); err != nil {
		t.Errorf("a named account = %v, want nil", err)
	}
	for _, u := range []*User{
		{Name: "dev", UID: ID(1000), GID: ID(1000)},
		{AdditionalGroups: []string{"docker"}},
	} {
		var refused *OneAccountError
		if err := u.ValidateAccount("windows"); !errors.As(err, &refused) {
			t.Errorf("ValidateAccount(%+v) = %v, want a OneAccountError", u, err)
		}
	}
}
