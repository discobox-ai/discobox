// Package runuser answers one question, for everything inside a sandbox that
// has to launch a process as somebody: given what each layer asked for, who
// does the process actually run as?
//
// Call Resolve with the layers and the fields you need. It returns a User with
// nothing left to work out for those fields -- no name to look up, no nil gid,
// no unresolved group -- so callers use the fields directly instead of each
// re-deriving them and drifting apart. That drift is what ADR 0025 was written
// about and what ADR 0033 removed the room for: terminals lost the sandbox's
// supplementary groups, the boot flow defaulted a missing uid to root and a
// missing gid to the uid, and an exec naming only a group either lost it or
// failed outright depending on how it was spelled.
//
// This package is the only one that resolves against the image's own
// /etc/passwd and /etc/group (ADR 0033 §6), which is the only place a sandbox's
// users and groups exist -- so it is usable only from inside the sandbox. The
// control plane and the pool agent cannot resolve these names and must not try
// (ADR 0025 §4); they use sandboxuser.Merge, which has no way to.
package runuser

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	osuser "os/user"
	"strconv"
	"strings"

	"github.com/obot-platform/discobox/sandboxuser"
)

// The identity vocabulary is shared with everything outside the sandbox, so it
// is one type rather than a parallel one that has to be converted (ADR 0025 §1).
type (
	User   = sandboxuser.User
	Layers = sandboxuser.Layers
	Fields = sandboxuser.Fields
)

// passwdPath is the account database the login-shell lookup parses. os/user
// does not expose the shell field, so it is read directly; it is a variable so
// tests can point it at a fixture.
var passwdPath = "/etc/passwd"

// The passwd/group database is reached through these indirections so tests can
// supply a fixed table instead of depending on whatever accounts the machine
// running them happens to have. effectiveIDs is here for the same reason: the
// image layer is "who this process already is", and a test that reads the real
// getuid can only ever assert it against itself.
var (
	lookupUserByName  = osuser.Lookup
	lookupUserByID    = osuser.LookupId
	lookupGroupByName = osuser.LookupGroup
	effectiveIDs      = func() (int64, int64) { return int64(os.Getuid()), int64(os.Getgid()) }
)

// Current is the image layer: who this process already is. Inside a sandbox
// that is the image's own account -- boot runs as PID 1 before anything has
// called setuid, and the agent's unit sets no User=, so the running ids are the
// ones the Dockerfile's USER directive selected.
//
// It reports ids only. Completing them to a name and home is Resolve's job, and
// whether that completion is required is the caller's to declare.
func Current() *User {
	uid, gid := effectiveIDs()
	return &User{UID: &uid, GID: &gid}
}

// Resolve merges the layers by precedence (sandboxuser.Merge) and then
// completes every field in need against the image's own account database.
//
// Completion asks; it never defaults (ADR 0025 §6). A name supplies both ids
// from its passwd entry; a uid with no group supplies the gid of that uid's
// entry -- its real default group, never 0 and never the uid, because uid==gid
// is a useradd coincidence rather than a rule. A named primary group resolves
// through the same path as a supplementary one, so the two cannot disagree.
//
// A field in need that cannot be determined is an *UnresolvedError naming it.
// There is no third outcome and in particular no zero standing in for an answer:
// 0 is root, "" is no home, and both read as answers at every call site
// downstream (ADR 0033 §2). A caller that knows a field is undeterminable in its
// context leaves it out of need, which is an explicit claim rather than a
// silent gap.
//
// Fields outside need are cleared rather than half-filled, so an unrequested
// field cannot be mistaken for a resolved one.
func Resolve(l Layers, need Fields) (User, error) {
	for _, layer := range []*User{l.Request, l.Manifest, l.Image} {
		if err := layer.Validate(); err != nil {
			return User{}, err
		}
	}
	user := sandboxuser.Merge(l)

	// The primary group first: a named one is the caller's explicit choice and
	// must not be quietly overtaken by the passwd entry's default below.
	if group := strings.TrimSpace(user.GroupName); group != "" {
		gid, ok := LookupGroupID(group)
		if !ok {
			return User{}, sandboxuser.Unresolved(sandboxuser.FieldGID,
				fmt.Sprintf("group %q is not a group in this image", group))
		}
		resolved := int64(gid)
		user.GID = &resolved
		user.GroupName = ""
	}

	// A name is the one input that can supply ids, so it is consulted whenever
	// something it could answer is still missing.
	if name := strings.TrimSpace(user.Name); name != "" && (user.UID == nil || user.GID == nil) {
		found, err := lookupUserByName(name)
		if err != nil {
			// An account the manifest names but the image has not created yet is
			// not an error in itself; it is only an error if it was needed to
			// answer something. Fall through and let the per-field checks decide.
			if !isUnknownUser(err) {
				return User{}, fmt.Errorf("resolve run user %q: %w", name, err)
			}
		} else {
			if user.UID == nil {
				uid, err := parseID(found.Uid, "uid", name)
				if err != nil {
					return User{}, err
				}
				user.UID = &uid
			}
			if user.GID == nil {
				gid, err := parseID(found.Gid, "gid", name)
				if err != nil {
					return User{}, err
				}
				user.GID = &gid
			}
		}
	}

	if need.Has(sandboxuser.FieldUID) && user.UID == nil {
		return User{}, sandboxuser.Unresolved(sandboxuser.FieldUID,
			"no layer named a uid and no name resolved to one")
	}

	// Only a uid can answer for the rest, so everything below needs one --
	// whether or not the caller asked for the uid itself.
	if user.UID != nil {
		if user.GID == nil && need.Has(sandboxuser.FieldGID) {
			found, err := lookupUserByID(strconv.FormatInt(*user.UID, 10))
			if err != nil {
				return User{}, sandboxuser.Unresolved(sandboxuser.FieldGID,
					fmt.Sprintf("uid %d has no passwd entry to take a primary group from", *user.UID))
			}
			gid, err := parseID(found.Gid, "gid", strconv.FormatInt(*user.UID, 10))
			if err != nil {
				return User{}, err
			}
			user.GID = &gid
		}
		if err := completeNameAndHome(&user, need); err != nil {
			return User{}, err
		}
	}

	clearUnrequested(&user, need)
	return user, nil
}

// completeNameAndHome fills the two descriptive fields from the uid's passwd
// entry. They come from the same entry and are filled together: a uid-only
// identity with neither leaves USER/LOGNAME/HOME unset in the process
// environment and resolves the login shell against a half-known user.
func completeNameAndHome(user *User, need Fields) error {
	wantName := need.Has(sandboxuser.FieldName) && strings.TrimSpace(user.Name) == ""
	wantHome := need.Has(sandboxuser.FieldHome) && strings.TrimSpace(user.HomeDirectory) == ""
	if !wantName && !wantHome {
		return nil
	}
	found, err := lookupUserByID(strconv.FormatInt(*user.UID, 10))
	if err != nil {
		field := sandboxuser.FieldName
		if !wantName {
			field = sandboxuser.FieldHome
		}
		return sandboxuser.Unresolved(field,
			fmt.Sprintf("uid %d has no passwd entry", *user.UID))
	}
	if wantName {
		if name := strings.TrimSpace(found.Username); name != "" {
			user.Name = name
		} else {
			return sandboxuser.Unresolved(sandboxuser.FieldName,
				fmt.Sprintf("uid %d has a passwd entry with no name", *user.UID))
		}
	}
	if wantHome {
		if home := strings.TrimSpace(found.HomeDir); home != "" {
			user.HomeDirectory = home
		} else {
			return sandboxuser.Unresolved(sandboxuser.FieldHome,
				fmt.Sprintf("uid %d has a passwd entry with no home directory", *user.UID))
		}
	}
	return nil
}

// clearUnrequested drops what the caller did not ask for. A field that was
// never completed must not travel on looking like an answer, and the caller
// said it does not need it.
func clearUnrequested(user *User, need Fields) {
	if !need.Has(sandboxuser.FieldUID) {
		user.UID = nil
	}
	if !need.Has(sandboxuser.FieldGID) {
		user.GID = nil
	}
	if !need.Has(sandboxuser.FieldName) {
		user.Name = ""
	}
	if !need.Has(sandboxuser.FieldHome) {
		user.HomeDirectory = ""
	}
	if !need.Has(sandboxuser.FieldGroups) {
		user.AdditionalGroups = nil
	}
}

func parseID(raw, kind, subject string) (int64, error) {
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("resolve run user %q %s %q: %w", subject, kind, raw, err)
	}
	return parsed, nil
}

func isUnknownUser(err error) bool {
	var unknownName osuser.UnknownUserError
	var unknownID osuser.UnknownUserIdError
	return errors.As(err, &unknownName) || errors.As(err, &unknownID)
}

// Groups resolves supplementary entries -- names or numeric GIDs -- to ids for
// a credential, dropping what the image never created and collapsing
// duplicates. Membership is the caller's to decide; the group file is consulted
// only to put a number to it (ADR 0025 §3).
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
	if len(out) == 0 {
		return nil
	}
	return out
}

// LookupGroupID turns one entry -- a group name or a numeric GID -- into a GID.
// A numeric entry resolves as an id before it is tried as a name, so a group
// literally named "997" cannot shadow gid 997, and a bare GID resolves even
// with no group-file line: the id is the authority and the file only names it.
func LookupGroupID(entry string) (uint32, bool) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return 0, false
	}
	if parsed, err := strconv.ParseInt(entry, 10, 64); err == nil {
		if parsed < 0 || parsed > int64(^uint32(0)) {
			return 0, false
		}
		return uint32(parsed), true
	}
	found, err := lookupGroupByName(entry)
	if err != nil {
		return 0, false
	}
	parsed, err := strconv.ParseInt(found.Gid, 10, 64)
	if err != nil || parsed < 0 || parsed > int64(^uint32(0)) {
		return 0, false
	}
	return uint32(parsed), true
}

// LoginShell reports the shell field of name's passwd entry, and whether the
// entry existed. os/user does not expose that field, so the database is parsed
// directly -- which is why this lives here rather than in execs: the format of
// /etc/passwd is knowledge this package already carries, and two packages
// should not each hold a copy of it (ADR 0033 §6).
//
// A missing entry is not an error. A user can run a process without one; the
// caller falls back to $SHELL and then to a probe of the usual paths.
func LoginShell(name string) (string, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false, nil
	}
	file, err := os.Open(passwdPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s: %w", passwdPath, err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) < 7 || fields[0] != name {
			continue
		}
		return strings.TrimSpace(fields[6]), true, nil
	}
	if err := scanner.Err(); err != nil {
		return "", false, fmt.Errorf("read %s: %w", passwdPath, err)
	}
	return "", false, nil
}
