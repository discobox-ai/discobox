package runuser

import osuser "os/user"

// FixedDatabase swaps the passwd/group lookups for a fixed table and returns a
// function that restores them. Tests in any package can use it (with
// t.Cleanup) so identity resolution is exercised against known entries rather
// than against whatever accounts the machine running the tests happens to have.
//
// It takes no *testing.T deliberately: importing testing from a non-test file
// would register test flags on every binary that links this package.
//
// The ids here break the uid==gid convention on purpose. That coincidence is
// exactly what the resolution rules must not rely on, so a fixture reproducing
// it would hide the bug rather than catch it.
func FixedDatabase() (restore func()) {
	users := map[string]osuser.User{
		"dev":  {Uid: "1000", Gid: "2000", Username: "dev", HomeDir: "/home/dev"},
		"root": {Uid: "0", Gid: "0", Username: "root", HomeDir: "/root"},
	}
	groups := map[string]osuser.Group{
		"dev":    {Gid: "2000", Name: "dev"},
		"docker": {Gid: "997", Name: "docker"},
		"video":  {Gid: "44", Name: "video"},
		"root":   {Gid: "0", Name: "root"},
	}
	byID := map[string]osuser.User{}
	for _, u := range users {
		byID[u.Uid] = u
	}

	prevName, prevID, prevGroup := lookupUserByName, lookupUserByID, lookupGroupByName
	lookupUserByName = func(name string) (*osuser.User, error) {
		if u, ok := users[name]; ok {
			return &u, nil
		}
		return nil, osuser.UnknownUserError(name)
	}
	lookupUserByID = func(uid string) (*osuser.User, error) {
		if u, ok := byID[uid]; ok {
			return &u, nil
		}
		return nil, osuser.UnknownUserIdError(0)
	}
	lookupGroupByName = func(name string) (*osuser.Group, error) {
		if g, ok := groups[name]; ok {
			return &g, nil
		}
		return nil, osuser.UnknownGroupError(name)
	}
	return func() { lookupUserByName, lookupUserByID, lookupGroupByName = prevName, prevID, prevGroup }
}
