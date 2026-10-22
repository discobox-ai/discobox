package sandboxuser

import (
	"fmt"
	"strconv"
	"strings"
)

// HasPOSIXIDs reports whether a sandbox whose platform's OS is goos -- spelled
// as GOOS spells it -- runs its processes by uid and gid. Linux is the one
// that does. Everywhere else a sandbox has exactly one account: the one its
// template provisions, named in its manifest, and nothing invents a uid or a
// gid for it (ADR 0145 §5).
func HasPOSIXIDs(goos string) bool { return goos == "linux" }

// OneAccountError refuses a layer that names something a single-account
// sandbox does not let it name: another user, a uid, a primary group, a group
// set, or a home directory. The home is refused even when it is the account's
// own, because it is the account's to have rather than a layer's to say: the
// manifest carries the account's name alone (ADR 0145 §5), and the home is
// read from the account itself. It is never quietly ignored, because an
// exec that asked to run as somebody and ran as somebody else has done the
// wrong thing whatever it printed (ADR 0145 §5).
type OneAccountError struct {
	// OS is the sandbox's platform's OS, as GOOS spells it.
	OS string
	// Account is the sandbox's one account, empty when the caller does not
	// know its name -- the control plane, which records intent and resolves
	// nothing (ADR 0025 §4).
	Account string
	// Field is the single field the layer named and may not.
	Field Fields
	// Named is what the layer gave for Field, for the message.
	Named string
}

func (e *OneAccountError) Error() string {
	account := "one account"
	if e.Account != "" {
		account = fmt.Sprintf("one account, %q,", e.Account)
	}
	return fmt.Sprintf("a %s sandbox has %s and no POSIX ids: a run user may not name %s (%s)",
		e.OS, account, oneAccountRefused[e.Field], e.Named)
}

// oneAccountRefused says what a layer named, for each field it may not.
var oneAccountRefused = map[Fields]string{
	FieldName:   "another account",
	FieldUID:    "a uid",
	FieldGID:    "a primary group",
	FieldGroups: "a group set",
	FieldHome:   "a home directory",
}

// ValidateOneAccount reports whether u says only what a sandbox on goos, which
// has a single account, can honor: that account by its name, or nothing at
// all. account is the account's name, or empty when the caller cannot know it,
// in which case any one name is accepted and only the facets no such sandbox
// has are refused. Names are compared as SameAccount compares them.
//
// The first offending field is reported, in the order a person reads a user:
// who, then its ids, then its groups, then its home.
func (u *User) ValidateOneAccount(goos, account string) error {
	if err := u.Validate(); err != nil || u == nil {
		return err
	}
	refuse := func(field Fields, named string) error {
		return &OneAccountError{OS: goos, Account: account, Field: field, Named: named}
	}
	if name := strings.TrimSpace(u.Name); name != "" && account != "" && !SameAccount(name, account) {
		return refuse(FieldName, strconv.Quote(name))
	}
	if u.UID != nil {
		return refuse(FieldUID, strconv.FormatInt(*u.UID, 10))
	}
	if u.GID != nil {
		return refuse(FieldGID, strconv.FormatInt(*u.GID, 10))
	}
	if group := strings.TrimSpace(u.GroupName); group != "" {
		return refuse(FieldGID, strconv.Quote(group))
	}
	if NamesGroups(u) {
		return refuse(FieldGroups, strings.Join(u.AdditionalGroups, ", "))
	}
	if home := strings.TrimSpace(u.HomeDirectory); home != "" {
		return refuse(FieldHome, strconv.Quote(home))
	}
	return nil
}

// SameAccount reports whether two names spell the same account on a platform
// that has one. Neither Windows nor macOS tells account names apart by case --
// a Windows logon as ada is Ada's, and a macOS directory lookup of ADA finds
// ada -- so a manifest or a request spelling the account in another case
// names it all the same.
func SameAccount(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}
