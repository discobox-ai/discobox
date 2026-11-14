package sandboxuser

import (
	"errors"
	"fmt"
)

// AccountIDs is what ids a guest OS gives the ordinary accounts it creates, and
// so which a sandbox's own account may be created with there (ADR 0141, ADR
// 26-11-12-060). Root, uid 0, is the one uid outside [Min, Max] a sandbox may
// name.
type AccountIDs struct {
	// Min and Max bound the range. Below Min are the guest's system accounts,
	// and above Max nothing it allocates.
	Min, Max int64
	// Group is the one gid outside the range a sandbox account may still take
	// as its primary group, and GroupName what a refusal calls it: root's on
	// Linux, staff on macOS, the group every account macOS makes is in.
	Group     int64
	GroupName string
	// SharedGroup is whether the guest puts every account it makes in Group,
	// as macOS does staff, rather than in a group of the account's own whose
	// gid is its uid, as useradd does on Linux.
	SharedGroup bool
}

// linuxAccountIDs are UID_MIN and UID_MAX in login.defs on the Debian the
// images build from. A macOS client's own uid 501 in group 20 lands below
// them, and 20 is dialout there (ADR 0141).
var linuxAccountIDs = AccountIDs{Min: 1000, Max: 60000, Group: 0, GroupName: "root"}

// darwinAccountIDs start at 501, the first id macOS gives an ordinary account,
// and admit gid 20, staff, the primary group of every account it makes. The
// upper bound is Linux's: macOS assigns upward from 501 (ADR 26-11-12-060 §1).
var darwinAccountIDs = AccountIDs{Min: 501, Max: 60000, Group: 20, GroupName: "staff", SharedGroup: true}

// AccountIDsFor is the ids a sandbox account on goos, spelled as GOOS spells
// it, may be created with. ok is false on a platform whose sandbox has no ids
// to create its account with: Windows, whose one account is named alone (ADR
// 0145 §5).
//
// It is wider than HasPOSIXIDs: a macOS sandbox's one account has POSIX ids,
// which create gives it, though the sandbox runs every process as that one
// account and an exec may name no other (ADR 26-10-09-106 §5).
func AccountIDsFor(goos string) (ids AccountIDs, ok bool) {
	switch goos {
	case "linux":
		return linuxAccountIDs, true
	case "darwin":
		return darwinAccountIDs, true
	}
	return AccountIDs{}, false
}

// InRange reports whether id is in [Min, Max]: one the guest gives an
// ordinary account. It is the range alone, without root or Group.
func (r AccountIDs) InRange(id int64) bool {
	return id >= r.Min && id <= r.Max
}

// ValidUID reports whether a sandbox account may be created with uid: root, or
// one in the range.
func (r AccountIDs) ValidUID(uid int64) bool {
	return uid == 0 || r.InRange(uid)
}

// ValidGID reports whether a sandbox account may be created with gid as its
// primary group: one in the range, or Group.
func (r AccountIDs) ValidGID(gid int64) bool {
	return gid == r.Group || r.InRange(gid)
}

// DefaultGID is the primary group the guest would give a new account with
// uid: Group where it shares one, else the account's own, numbered as its uid.
// It is what a client sends in place of a gid the guest does not give an
// account (ADR 0141 §2, ADR 26-11-12-060 §1).
func (r AccountIDs) DefaultGID(uid int64) int64 {
	if r.SharedGroup {
		return r.Group
	}
	return uid
}

// errAccountWithoutUID is a sandbox user named without the uid to create it
// with. The pool agent owns a source tree's contents before the sandbox exists
// and chowns them to that uid; with none it leaves them root's (ADR 0141).
var errAccountWithoutUID = errors.New("a sandbox user that names an account must give its uid")

// ValidateAccount reports whether u can be the own user of a sandbox whose
// platform's OS is goos -- the one a sandbox create records and boot
// provisions -- on top of Validate's in-layer check. It is stricter than an
// exec's user, which names an account the sandbox already has and may be
// anyone in it.
//
// Where the guest's accounts have ids (AccountIDsFor), naming an account
// requires its uid, the uid must be root or in the guest's range, and the gid
// in that range or its Group. An id outside them is an error, never
// clamped: which id to use instead is the caller's to choose, and a client
// that has no usable one of its own picks it before sending (ADR 0141, ADR
// 26-11-12-060). A user that names only groups keeps the image's account and
// needs no uid.
//
// Off Linux the sandbox has one account, which its template provisions and
// its manifest names, so the user may name that account and nothing else: no
// groups and no home (ValidateOneAccount, ADR 0145 §5). On macOS that account
// has ids, which it may give as above; on Windows it has none, so it may give
// no ids either (ADR 26-10-09-106 §5, ADR 26-11-12-060 §1).
func (u *User) ValidateAccount(goos string) error {
	ids, ok := AccountIDsFor(goos)
	if !HasPOSIXIDs(goos) {
		rest := u.Clone()
		if ok && rest != nil {
			rest.UID, rest.GID = nil, nil
		}
		if err := rest.ValidateOneAccount(goos, ""); err != nil || !ok {
			return err
		}
	}
	if err := u.Validate(); err != nil || u == nil {
		return err
	}
	if NamesIdentity(u) && u.UID == nil {
		return errAccountWithoutUID
	}
	if u.UID != nil && !ids.ValidUID(*u.UID) {
		return ids.refuse(goos, "uid", *u.UID, 0, "root")
	}
	if u.GID != nil && !ids.ValidGID(*u.GID) {
		return ids.refuse(goos, "gid", *u.GID, ids.Group, ids.GroupName)
	}
	return nil
}

// refuse names the id, the platform and the ids that platform does take for
// the field: the one outside the range, then the range.
func (r AccountIDs) refuse(goos, field string, id, other int64, otherName string) error {
	return fmt.Errorf("%s %d is outside the ids a %s sandbox account may use: %d (%s) or %d-%d",
		field, id, goos, other, otherName, r.Min, r.Max)
}
