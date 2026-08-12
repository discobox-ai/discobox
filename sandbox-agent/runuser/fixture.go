package runuser

import (
	"os"
	osuser "os/user"
	"path/filepath"
)

// FixedDatabase swaps the account database for a fixed table and returns a
// restore function; use it with t.Cleanup from any package.
//
// It takes no *testing.T on purpose: importing testing from a non-test file
// registers test flags on every binary that links the package.
//
// The ids deliberately break uid == gid. A fixture reproducing that coincidence
// would hide the bug this package exists to prevent rather than catch it -- and
// that includes the effective ids, which are pinned here too. While they were
// read from the real process, a test could only assert them against another
// call to os.Getuid, which passes for any implementation and cannot tell a uid
// from a gid on the usual developer account where the two are equal.
//
// The passwd *file* is written as well as the lookups, because the login-shell
// field is parsed out of it directly (os/user does not expose it).
func FixedDatabase() (restore func()) {
	users := map[string]osuser.User{
		"dev":  {Uid: "1000", Gid: "2000", Username: "dev", HomeDir: "/home/dev"},
		"root": {Uid: "0", Gid: "0", Username: "root", HomeDir: "/root"},
		// The account the fixture's effective ids belong to, so "who is this
		// process" resolves inside the fixture rather than falling through to
		// the host's real database.
		"image": {Uid: "1500", Gid: "1600", Username: "image", HomeDir: "/home/image"},
	}
	groups := map[string]osuser.Group{
		"dev":    {Gid: "2000", Name: "dev"},
		"docker": {Gid: "997", Name: "docker"},
		"video":  {Gid: "44", Name: "video"},
		"root":   {Gid: "0", Name: "root"},
		"image":  {Gid: "1600", Name: "image"},
	}
	byID := map[string]osuser.User{}
	for _, u := range users {
		byID[u.Uid] = u
	}

	shells := map[string]string{
		"dev":   "/usr/bin/zsh",
		"root":  "/bin/sh",
		"image": "/bin/bash",
	}
	dir, err := os.MkdirTemp("", "runuser-passwd")
	var passwdFile string
	if err == nil {
		passwdFile = filepath.Join(dir, "passwd")
		var content []byte
		for name, u := range users {
			content = append(content, []byte(name+":x:"+u.Uid+":"+u.Gid+"::"+u.HomeDir+":"+shells[name]+"\n")...)
		}
		if os.WriteFile(passwdFile, content, 0o600) != nil {
			passwdFile = ""
		}
	}

	prevName, prevID, prevGroup := lookupUserByName, lookupUserByID, lookupGroupByName
	prevIDs, prevPasswd := effectiveIDs, passwdPath
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
	effectiveIDs = func() (int64, int64) { return 1500, 1600 }
	if passwdFile != "" {
		passwdPath = passwdFile
	}
	return func() {
		lookupUserByName, lookupUserByID, lookupGroupByName = prevName, prevID, prevGroup
		effectiveIDs, passwdPath = prevIDs, prevPasswd
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
	}
}

// FixedEffectiveIDs overrides just the effective ids, for a test that needs the
// image layer to be an account the fixed database does not know -- an image
// running as a uid with no passwd entry, which is a real and awkward case.
func FixedEffectiveIDs(uid, gid int64) (restore func()) {
	prev := effectiveIDs
	effectiveIDs = func() (int64, int64) { return uid, gid }
	return func() { effectiveIDs = prev }
}
