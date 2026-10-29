package cli

import (
	"fmt"
	"strings"
)

// rawFlag is --raw: how a binding hands a discobox a secret's value itself
// rather than a sentinel (ADR 26-10-27-840), env (in the variable) or file (a
// file whose path is in it). none, which only a command changing an existing
// binding takes (canClear), makes it a sentinel again.
type rawFlag struct {
	value    string
	set      bool
	canClear bool
}

func (f *rawFlag) Set(text string) error {
	switch text = strings.TrimSpace(text); {
	case text == "env" || text == "file":
	case text == "none" && f.canClear:
	case f.canClear:
		return fmt.Errorf("%q is not a delivery: env, file, or none for a sentinel", text)
	default:
		return fmt.Errorf("%q is not a delivery: env or file", text)
	}
	f.value, f.set = text, true
	return nil
}

// String is empty until set, so help prints no default: left out is a
// sentinel, as each usage says.
func (f *rawFlag) String() string { return f.value }

func (f *rawFlag) Type() string { return "delivery" }
