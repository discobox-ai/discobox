// Package runuser answers one question, for everything inside a sandbox that
// has to launch a process as somebody: given what a caller asked for, who does
// the process actually run as?
//
// Call Resolve. It returns a User with nothing left to work out -- no name to
// look up, no nil gid, no unresolved group -- so callers can use the fields
// directly instead of each re-deriving them and drifting apart. That drift is
// what ADR 0025 was written about: terminals lost the sandbox's supplementary
// groups, the boot flow defaulted a missing uid to root and a missing gid to the
// uid, and an exec that named a user ran with no groups at all.
//
// Resolution reads the image's own /etc/passwd and /etc/group, which is the
// only place a sandbox's users and groups exist -- so this package is usable
// only from inside the sandbox. The control plane and the pool agent cannot
// resolve these names and must not try (ADR 0025 §4).
package runuser

import (
	"errors"
	"fmt"
	osuser "os/user"
	"strconv"
	"strings"
)

// User is a run identity: who to be, and which groups to carry. Every field is
// optional on the way in; Resolve fills in whatever the caller left out.
type User struct {
	Name string `json:"name,omitempty"`
	UID  *int64 `json:"uid,omitempty"`
	GID  *int64 `json:"gid,omitempty"`
	// Group is the primary group by name, mutually exclusive with GID. Resolve
	// turns it into GID and clears it, so nothing downstream has to know which
	// of the two a caller supplied.
	Group         string `json:"group,omitempty"`
	HomeDirectory string `json:"homeDirectory,omitempty"`
	// AdditionalGroups are supplementary groups, each a group name or a numeric
	// GID. Whoever supplied the list is the authority on membership; the group
	// file is consulted only to resolve an entry to an id (ADR 0025 §3).
	AdditionalGroups []string `json:"additionalGroups,omitempty"`
}

// Empty reports whether a user names nobody to run as. Groups are deliberately
// not considered: a request carrying only groups still has to borrow an
// identity from somewhere, which is what lets callers express "the usual user,
// plus these groups" (ADR 0025 §2).
func (u *User) Empty() bool {
	return u == nil || strings.TrimSpace(u.Name) == "" && u.UID == nil && u.GID == nil && strings.TrimSpace(u.HomeDirectory) == ""
}

// Clone returns a deep copy, so a caller's slice cannot be mutated through the
// copy or vice versa.
func (u *User) Clone() *User {
	if u.Empty() {
		return nil
	}
	out := *u
	out.Name = strings.TrimSpace(out.Name)
	out.Group = strings.TrimSpace(out.Group)
	out.HomeDirectory = strings.TrimSpace(out.HomeDirectory)
	out.UID = cloneInt64(u.UID)
	out.GID = cloneInt64(u.GID)
	out.AdditionalGroups = append([]string(nil), u.AdditionalGroups...)
	return &out
}

// The passwd/group database is reached through these indirections so tests can
// supply a fixed table instead of depending on whatever accounts the machine
// running them happens to have.
var (
	lookupUserByName  = osuser.Lookup
	lookupUserByID    = osuser.LookupId
	lookupGroupByName = osuser.LookupGroup
)

// Resolve fills in everything the caller left out, by asking the OS rather than
// defaulting it (ADR 0025 §6). A name supplies both ids from its passwd entry; a
// uid with no group supplies the gid of that uid's entry -- its real default
// group, never 0 and never the uid, because uid==gid is a useradd coincidence
// rather than a rule. A named primary group resolves through the same path as
// the supplementary ones, so they cannot resolve by different rules.
//
// Only what is missing is looked up. A user that already carries its ids, name,
// and home needs no account to exist yet, which matters because the boot flow
// resolves identities for accounts it is about to create.
//
// An error means the identity cannot be honored -- an unknown name, a uid with
// no passwd entry, or gid and group name given together. Callers must not fall
// back to a default on error; that is the guess this package exists to remove.
func Resolve(user User) (User, error) {
	if user.Empty() {
		return user, nil
	}
	needsGroup := user.GID == nil && strings.TrimSpace(user.Group) == ""
	if name := strings.TrimSpace(user.Name); name != "" && (user.UID == nil || needsGroup) {
		found, err := lookupUserByName(name)
		if err != nil {
			return user, fmt.Errorf("resolve run user %q: %w", name, err)
		}
		if user.UID == nil {
			uid, err := strconv.ParseInt(found.Uid, 10, 64)
			if err != nil {
				return user, fmt.Errorf("resolve run user %q uid %q: %w", name, found.Uid, err)
			}
			user.UID = &uid
		}
		if needsGroup {
			gid, err := strconv.ParseInt(found.Gid, 10, 64)
			if err != nil {
				return user, fmt.Errorf("resolve run user %q gid %q: %w", name, found.Gid, err)
			}
			user.GID = &gid
		}
	}
	if group := strings.TrimSpace(user.Group); group != "" {
		if user.GID != nil {
			return user, errors.New("run user gid and group are mutually exclusive")
		}
		gid, ok := LookupGroupID(group)
		if !ok {
			return user, fmt.Errorf("resolve run user group %q: no such group", group)
		}
		resolved := int64(gid)
		user.GID = &resolved
		user.Group = ""
	}
	if user.UID == nil {
		return user, errors.New("run user uid is required")
	}
	if user.GID == nil {
		found, err := lookupUserByID(strconv.FormatInt(*user.UID, 10))
		if err != nil {
			return user, fmt.Errorf("resolve run user uid %d primary group: %w", *user.UID, err)
		}
		gid, err := strconv.ParseInt(found.Gid, 10, 64)
		if err != nil {
			return user, fmt.Errorf("resolve run user uid %d gid %q: %w", *user.UID, found.Gid, err)
		}
		user.GID = &gid
	}
	// Name and home come from the same entry. Without them a uid-only identity
	// leaves USER/LOGNAME/HOME unset in the process environment and resolves the
	// login shell against a half-known user, so "resolved" has to mean all four
	// fields rather than just the ids.
	if strings.TrimSpace(user.Name) == "" || strings.TrimSpace(user.HomeDirectory) == "" {
		name, home, err := NameAndHome(&user)
		if err != nil {
			return user, err
		}
		if strings.TrimSpace(user.Name) == "" {
			user.Name = name
		}
		if strings.TrimSpace(user.HomeDirectory) == "" {
			user.HomeDirectory = home
		}
	}
	return user, nil
}

// NameAndHome resolves the effective login name and home directory. Values set
// explicitly win; anything unset is filled from the passwd database by name,
// then by UID.
//
// A name that does not exist is an error; a UID that does not resolve is
// reported as unknown (empty) rather than fatal, since a bare UID can still run
// a process without a passwd entry.
func NameAndHome(user *User) (name, home string, err error) {
	if user.Empty() {
		return "", "", nil
	}
	name = strings.TrimSpace(user.Name)
	home = strings.TrimSpace(user.HomeDirectory)
	switch {
	case name != "":
		found, lookupErr := lookupUserByName(name)
		if lookupErr != nil {
			return "", "", fmt.Errorf("resolve run user %q: %w", name, lookupErr)
		}
		if home == "" {
			home = strings.TrimSpace(found.HomeDir)
		}
	case user.UID != nil:
		if found, lookupErr := lookupUserByID(strconv.FormatInt(*user.UID, 10)); lookupErr == nil {
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

// Groups resolves supplementary group entries to the GIDs a process runs with,
// dropping any the image never created. A missing group is skipped rather than
// fatal, mirroring the boot flow -- the two must not disagree about the same
// image, and a harness Dockerfile that forgot to install a package must not
// break every process in the sandbox.
func Groups(entries []string) []uint32 {
	if len(entries) == 0 {
		return nil
	}
	seen := make(map[uint32]struct{}, len(entries))
	out := make([]uint32, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		gid, ok := LookupGroupID(entry)
		if !ok {
			continue
		}
		if _, dup := seen[gid]; dup {
			continue
		}
		seen[gid] = struct{}{}
		out = append(out, gid)
	}
	return out
}

// LookupGroupID resolves one group entry -- a name or a numeric GID -- to a GID.
// Numeric is tried first, so a group literally named "997" cannot shadow gid
// 997, and a bare GID resolves even with no group-file line: the id is the
// authority and the file only names it.
func LookupGroupID(entry string) (uint32, bool) {
	if parsed, err := strconv.ParseInt(entry, 10, 64); err == nil {
		if parsed < 0 || parsed > int64(^uint32(0)) {
			return 0, false
		}
		return uint32(parsed), true
	}
	group, err := lookupGroupByName(entry)
	if err != nil {
		return 0, false
	}
	parsed, err := strconv.ParseInt(group.Gid, 10, 64)
	if err != nil || parsed < 0 || parsed > int64(^uint32(0)) {
		return 0, false
	}
	return uint32(parsed), true
}

func cloneInt64(in *int64) *int64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
