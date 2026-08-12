package sandboxuser

import (
	"errors"
	"fmt"
)

// errBothGIDAndGroupName is the one contradiction a single layer can carry:
// two answers to "which primary group", with no way to prefer one.
var errBothGIDAndGroupName = errors.New("gid and groupName are mutually exclusive")

// UnresolvedError reports a field a caller required that could not be
// determined from the layers it supplied plus the account database available to
// it. It is deliberately not a fallback: the caller either has standing to ask
// for the field or it does not, and a default here would be the guess this
// whole design exists to remove (ADR 0032 §2).
type UnresolvedError struct {
	// Field is the single field that could not be resolved.
	Field Fields
	// Reason says what was missing, in terms of what the caller could fix.
	Reason string
}

func (e *UnresolvedError) Error() string {
	return fmt.Sprintf("cannot resolve run user %s: %s", e.Field, e.Reason)
}

// Unresolved builds an UnresolvedError for one field.
func Unresolved(field Fields, reason string) error {
	return &UnresolvedError{Field: field, Reason: reason}
}
