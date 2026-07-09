package execs

import (
	"fmt"
	osuser "os/user"
	"strconv"
	"strings"
)

// ResolveUser resolves the effective login name and home directory for a run
// user. Values set explicitly on user win; anything unset is filled in from the
// OS user database (/etc/passwd) by name, then by UID. It is the single source
// of truth for user/home resolution shared by process env defaults and agent
// file installation, so the two cannot drift.
//
// An empty user yields empty results. A name that does not exist is an error; a
// UID that does not resolve is treated as "unknown" (empty), since a bare UID
// can still run a process without a passwd entry.
func ResolveUser(user *User) (name, home string, err error) {
	if emptyUser(user) {
		return "", "", nil
	}
	name = strings.TrimSpace(user.Name)
	home = strings.TrimSpace(user.HomeDirectory)
	switch {
	case name != "":
		found, lookupErr := osuser.Lookup(name)
		if lookupErr != nil {
			return "", "", fmt.Errorf("resolve exec user %q: %w", name, lookupErr)
		}
		if home == "" {
			home = strings.TrimSpace(found.HomeDir)
		}
	case user.UID != nil:
		if found, lookupErr := osuser.LookupId(strconv.FormatInt(*user.UID, 10)); lookupErr == nil {
			if name == "" {
				name = strings.TrimSpace(found.Username)
			}
			if home == "" {
				home = strings.TrimSpace(found.HomeDir)
			}
		}
	}
	return name, home, nil
}
