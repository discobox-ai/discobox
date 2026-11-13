package runuser

import (
	"errors"
	"os"
	osuser "os/user"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func withAccount(t *testing.T, u *osuser.User, err error) {
	t.Helper()
	old := lookupUser
	lookupUser = func(string) (*osuser.User, error) { return u, err }
	t.Cleanup(func() { lookupUser = old })
}

// The primary group leads the set and is not repeated, and the set is cut to
// what the platform lets a process hold: on macOS an admin account's twenty
// groups would otherwise fail setgroups.
func TestAnAccountsGroupSetLeadsWithItsPrimaryAndFitsThePlatform(t *testing.T) {
	ids := []string{"20", "12", "61", "79", "80", "81", "98", "701", "33", "100", "204", "250", "395", "398", "399", "400", "401", "402", "403"}
	got := groupSet(20, ids, 16)
	if len(got) != 16 || got[0] != 20 || slices.Index(got[1:], 20) >= 0 {
		t.Fatalf("groupSet = %v, want 16 groups, 20 first and once", got)
	}
	if got := groupSet(20, []string{"x", "12"}, 16); !slices.Equal(got, []int{20, 12}) {
		t.Fatalf("groupSet = %v, want what parses", got)
	}
}

// Root is never an account to drop to, whatever names it.
func TestLookupAccountRefusesRoot(t *testing.T) {
	withAccount(t, &osuser.User{Username: "toor", Uid: "0", Gid: "0"}, nil)
	if _, err := LookupAccount("toor"); err == nil || !strings.Contains(err.Error(), "uid 0") {
		t.Fatalf("LookupAccount(toor) = %v, want root refused", err)
	}
}

func TestLookupAccountNamesWhatItCannotFind(t *testing.T) {
	if _, err := LookupAccount(" "); err == nil {
		t.Fatal("an empty name was looked up")
	}
	withAccount(t, nil, errors.New("unknown user ghost"))
	if _, err := LookupAccount("ghost"); err == nil || !strings.Contains(err.Error(), `"ghost"`) {
		t.Fatalf("LookupAccount(ghost) = %v", err)
	}
}

// The image layer is who the process is now, not who it was first asked as:
// an agent started as root resolves its user before it drops to the
// sandbox's account (boot.Provision) and again after, and os/user.Current
// would answer root both times, so a dropped macOS agent refused its own
// account and launchd restarted it in a loop. Changing the effective uid
// between the two asks must change the account currentAccount reads.
func TestTheImageAccountFollowsAChangeOfUID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX uid and nothing there drops to an account")
	}
	if os.Geteuid() == 0 {
		t.Skip("needs a uid that is not root's, to change it to root's")
	}
	before := currentAccount()
	if before == nil || before.Name == "" || before.Name == "root" {
		t.Fatalf("the image account as this test's user = %+v", before)
	}
	restore := FixedEffectiveIDs(0, 0)
	defer restore()
	if after := currentAccount(); after == nil || after.Name != "root" {
		t.Fatalf("after the uid became 0, the image account = %+v, want root: an answer cached from before the change", after)
	}
}
