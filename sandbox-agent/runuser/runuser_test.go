package runuser

import (
	"errors"
	"fmt"
	"testing"

	"github.com/obot-platform/discobox/sandboxuser"
)

// Completion, given a single layer. Precedence between layers is
// sandboxuser.Merge's matrix; this is the half that asks the account database.
func TestResolveCompletesAgainstTheAccountDatabase(t *testing.T) {
	for name, tc := range map[string]struct {
		in      User
		need    Fields
		wantUID int64
		wantGID int64
		wantErr bool
	}{
		// A name supplies both ids, and the gid is the account's real default
		// group -- 2000, not the uid.
		"name alone": {in: User{Name: "dev"}, wantUID: 1000, wantGID: 2000},
		// A uid with no group takes that uid's default group, never the uid and
		// never 0.
		"uid alone": {in: User{UID: sandboxuser.ID(1000)}, wantUID: 1000, wantGID: 2000},
		// A named primary group resolves through the same path as the
		// supplementary ones.
		"name and group name": {in: User{Name: "dev", GroupName: "docker"}, wantUID: 1000, wantGID: 997},
		"uid and group name":  {in: User{UID: sandboxuser.ID(1000), GroupName: "video"}, wantUID: 1000, wantGID: 44},
		// Explicit ids are taken as given, with no lookup at all.
		"both ids given": {in: User{UID: sandboxuser.ID(1000), GID: sandboxuser.ID(5)}, wantUID: 1000, wantGID: 5},
		// Root is an account like any other; it is never a fallback.
		"root by name": {in: User{Name: "root"}, wantUID: 0, wantGID: 0},

		"gid and group name together": {in: User{UID: sandboxuser.ID(1000), GID: sandboxuser.ID(5), GroupName: "docker"}, wantErr: true},
		"unknown group name":          {in: User{UID: sandboxuser.ID(1000), GroupName: "no-such-group"}, wantErr: true},
		// A name the image does not have cannot supply a uid, and a uid is
		// needed here, so the shortfall surfaces as an error rather than a zero.
		"unknown user name":   {in: User{Name: "nobody-here"}, wantErr: true},
		"uid with no account": {in: User{UID: sandboxuser.ID(4242424)}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(FixedDatabase())
			need := tc.need
			if need == 0 {
				need = sandboxuser.Credential
			}
			got, err := Resolve(Layers{Manifest: &tc.in}, need)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Resolve = %#v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.UID == nil || *got.UID != tc.wantUID {
				t.Fatalf("uid = %v, want %d", got.UID, tc.wantUID)
			}
			if got.GID == nil || *got.GID != tc.wantGID {
				t.Fatalf("gid = %v, want %d", got.GID, tc.wantGID)
			}
			// Once resolved, GID carries the answer and GroupName is cleared, so
			// nothing downstream has to know which form the caller used.
			if got.GroupName != "" {
				t.Fatalf("groupName = %q, want cleared once resolved", got.GroupName)
			}
		})
	}
}

// A field left out of `need` is neither looked up nor returned. This is the
// property the whole design rests on: a caller that cannot determine something
// says so, and gets absence rather than a zero that reads like an answer at
// every site downstream (ADR 0033 §2).
func TestResolveOnlyAnswersWhatWasAsked(t *testing.T) {
	t.Cleanup(FixedDatabase())

	// A uid with no passwd entry cannot yield a name or a home...
	orphan := Layers{Manifest: &User{UID: sandboxuser.ID(4242424), GID: sandboxuser.ID(7)}}
	if _, err := Resolve(orphan, sandboxuser.Complete); err == nil {
		t.Fatal("asking for a name the database cannot supply must fail")
	}
	// ...but a caller that does not need them is served.
	got, err := Resolve(orphan, sandboxuser.FieldUID|sandboxuser.FieldGID)
	if err != nil {
		t.Fatalf("credential-only resolve: %v", err)
	}
	if got.UID == nil || *got.UID != 4242424 || got.GID == nil || *got.GID != 7 {
		t.Fatalf("ids = %v/%v, want 4242424/7", got.UID, got.GID)
	}
	if got.Name != "" || got.HomeDirectory != "" {
		t.Fatalf("unrequested fields came back populated: name=%q home=%q", got.Name, got.HomeDirectory)
	}

	// The same clearing applies to fields that *could* have been resolved: an
	// unrequested field must not travel on looking like an answer.
	got, err = Resolve(Layers{Manifest: &User{Name: "dev"}}, sandboxuser.FieldUID)
	if err != nil {
		t.Fatalf("uid-only resolve: %v", err)
	}
	if got.UID == nil || *got.UID != 1000 {
		t.Fatalf("uid = %v, want 1000", got.UID)
	}
	if got.GID != nil || got.Name != "" || got.HomeDirectory != "" {
		t.Fatalf("unrequested fields survived: %#v", got)
	}
}

// An unresolved field names itself, so the error lands on whoever can fix it
// rather than surfacing later as a blank.
func TestResolveNamesTheFieldItCouldNotResolve(t *testing.T) {
	t.Cleanup(FixedDatabase())
	_, err := Resolve(Layers{Manifest: &User{Name: "dev"}}, sandboxuser.Complete)
	if err != nil {
		t.Fatalf("dev resolves fully: %v", err)
	}

	_, err = Resolve(Layers{Manifest: &User{GroupName: "docker"}}, sandboxuser.Credential)
	var unresolved *sandboxuser.UnresolvedError
	if !errors.As(err, &unresolved) {
		t.Fatalf("err = %v, want an UnresolvedError", err)
	}
	if unresolved.Field != sandboxuser.FieldUID {
		t.Fatalf("field = %s, want uid", unresolved.Field)
	}
}

// Current is the image layer. It is pinned by the fixture precisely so a test
// cannot assert it against another call to os.Getuid, which would pass for any
// implementation -- including one that transposed uid and gid, invisible on the
// usual developer account where the two are equal.
func TestCurrentIsTheImageIdentity(t *testing.T) {
	t.Cleanup(FixedDatabase())
	current := Current()
	if current.UID == nil || *current.UID != 1500 {
		t.Fatalf("uid = %v, want the fixture's 1500", current.UID)
	}
	if current.GID == nil || *current.GID != 1600 {
		t.Fatalf("gid = %v, want the fixture's 1600, which is deliberately not the uid", current.GID)
	}

	resolved, err := Resolve(Layers{Image: current}, sandboxuser.Complete)
	if err != nil {
		t.Fatalf("resolve the image identity: %v", err)
	}
	if resolved.Name != "image" || resolved.HomeDirectory != "/home/image" {
		t.Fatalf("resolved = %+v, want the image account", resolved)
	}
}

// An image running as a uid with no passwd entry is a real case, and one the
// caller has to be told about rather than handed a blank name for.
func TestResolveReportsAnImageUserWithNoAccount(t *testing.T) {
	t.Cleanup(FixedDatabase())
	t.Cleanup(FixedEffectiveIDs(4242424, 4242424))
	if _, err := Resolve(Layers{Image: Current()}, sandboxuser.Complete); err == nil {
		t.Fatal("an image uid with no passwd entry must not resolve to a blank name")
	}
}

// The login shell comes from the passwd entry, which os/user cannot report.
func TestLoginShellReadsThePasswdEntry(t *testing.T) {
	t.Cleanup(FixedDatabase())
	shell, found, err := LoginShell("dev")
	if err != nil || !found || shell != "/usr/bin/zsh" {
		t.Fatalf("LoginShell(dev) = %q,%v,%v want /usr/bin/zsh,true,nil", shell, found, err)
	}
	if _, found, _ := LoginShell("nobody-here"); found {
		t.Fatal("an absent account reported an entry")
	}
	if _, found, _ := LoginShell(""); found {
		t.Fatal("an empty name reported an entry")
	}
}

// A group entry is a name or a numeric GID. A numeric entry resolves as an id
// even when no group line names it -- the id is the authority, and the group
// file merely names it.
func TestLookupGroupID(t *testing.T) {
	t.Cleanup(FixedDatabase())
	for entry, want := range map[string]int64{"docker": 997, "video": 44, "4242": 4242, "0": 0} {
		got, ok := LookupGroupID(entry)
		if !ok || int64(got) != want {
			t.Fatalf("LookupGroupID(%q) = %d,%v, want %d,true", entry, got, ok, want)
		}
	}
	for _, entry := range []string{"no-such-group", "-1", fmt.Sprint(int64(^uint32(0)) + 1)} {
		if got, ok := LookupGroupID(entry); ok {
			t.Fatalf("LookupGroupID(%q) = %d,true, want not found", entry, got)
		}
	}
}
