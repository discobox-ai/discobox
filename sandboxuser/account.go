package sandboxuser

import (
	"errors"
	"fmt"
)

// AccountIDMin and AccountIDMax bound the ids a sandbox's own account may be
// created with: UID_MIN and UID_MAX in login.defs on the Debian the images build
// from. Below them are the guest's system accounts -- a macOS client's own uid
// 501 in group 20 would land there, and 20 is dialout -- and above them nothing
// the guest allocates. Root, 0, is the one id outside them a sandbox may name
// (ADR 0141).
const (
	AccountIDMin int64 = 1000
	AccountIDMax int64 = 60000
)

// InAccountRange reports whether id is one the guest gives ordinary accounts.
// It is the range alone; ValidateAccount also admits root.
func InAccountRange(id int64) bool {
	return id >= AccountIDMin && id <= AccountIDMax
}

// errAccountWithoutUID is a sandbox user named without the uid to create it
// with. The pool agent owns a source tree's contents before the sandbox exists
// and chowns them to that uid; with none it leaves them root's (ADR 0141).
var errAccountWithoutUID = errors.New("a sandbox user that names an account must give its uid")

// ValidateAccount reports whether u can be a sandbox's own user -- the one a
// sandbox create records and boot provisions -- on top of Validate's in-layer
// check. It is stricter than an exec's user, which names an account the sandbox
// already has and may be anyone in it.
//
// Naming an account requires its uid, and each id must be root or in
// [AccountIDMin, AccountIDMax]. An id outside them is an error, never clamped:
// which id to use instead is the caller's to choose, and a client that has no
// usable one of its own picks it before sending (ADR 0141). A user that names
// only groups keeps the image's account and needs no uid.
func (u *User) ValidateAccount() error {
	if err := u.Validate(); err != nil || u == nil {
		return err
	}
	if NamesIdentity(u) && u.UID == nil {
		return errAccountWithoutUID
	}
	if err := validateAccountID("uid", u.UID); err != nil {
		return err
	}
	return validateAccountID("gid", u.GID)
}

func validateAccountID(field string, id *int64) error {
	if id == nil || *id == 0 || InAccountRange(*id) {
		return nil
	}
	return fmt.Errorf("%s %d is outside the range a sandbox account may use: 0 (root) or %d-%d", field, *id, AccountIDMin, AccountIDMax)
}
