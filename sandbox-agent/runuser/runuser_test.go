package runuser

import (
	"fmt"
	"testing"
)

func int64Ptr(v int64) *int64 { return &v }

func TestResolve(t *testing.T) {
	for name, tc := range map[string]struct {
		in      User
		wantUID int64
		wantGID int64
		wantErr bool
	}{
		// A name supplies both ids, and the gid is the account's real default
		// group -- 2000, not the uid.
		"name alone": {in: User{Name: "dev"}, wantUID: 1000, wantGID: 2000},
		// A uid with no group takes that uid's default group, never the uid and
		// never 0.
		"uid alone": {in: User{UID: int64Ptr(1000)}, wantUID: 1000, wantGID: 2000},
		// A named primary group resolves through the same path as the
		// supplementary ones.
		"name and group name": {in: User{Name: "dev", Group: "docker"}, wantUID: 1000, wantGID: 997},
		"uid and group name":  {in: User{UID: int64Ptr(1000), Group: "video"}, wantUID: 1000, wantGID: 44},
		// Explicit ids are taken as given, with no lookup at all.
		"both ids given": {in: User{UID: int64Ptr(1000), GID: int64Ptr(5)}, wantUID: 1000, wantGID: 5},
		// Root is an account like any other; it is never a fallback.
		"root by name": {in: User{Name: "root"}, wantUID: 0, wantGID: 0},

		"gid and group name together": {in: User{UID: int64Ptr(1000), GID: int64Ptr(5), Group: "docker"}, wantErr: true},
		"unknown user name":           {in: User{Name: "nobody-here"}, wantErr: true},
		"unknown group name":          {in: User{UID: int64Ptr(1000), Group: "no-such-group"}, wantErr: true},
		"uid with no account":         {in: User{UID: int64Ptr(4242424)}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(FixedDatabase())
			user := tc.in
			err := func() error { got, err := Resolve(user); user = got; return err }()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveUserIdentity = %#v, want an error", user)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveUserIdentity: %v", err)
			}
			if user.UID == nil || *user.UID != tc.wantUID {
				t.Fatalf("uid = %v, want %d", user.UID, tc.wantUID)
			}
			if user.GID == nil || *user.GID != tc.wantGID {
				t.Fatalf("gid = %v, want %d", user.GID, tc.wantGID)
			}
			// Once resolved, GID carries the answer and Group is cleared, so
			// nothing downstream has to know which form the caller used.
			if user.Group != "" {
				t.Fatalf("group = %q, want cleared once resolved", user.Group)
			}
		})
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
