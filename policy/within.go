package policy

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// Unsettled is an allow that some allows of the policy it is held to cover
// in every part but their conditions: whether its conditions are within one
// of theirs is a judge's to say (ADR 26-10-20-782 §11). It is within if they
// are within any one. Delegated names the allows of that policy — a
// delegation's, or an approval rule's.
type Unsettled struct {
	Given     Statement
	Delegated []Statement
}

// Within reports whether given is within delegated, the policy it is held
// to: a policy handed on within the policy of the delegation it is handed on
// under (ADR 26-10-20-782 §11), or the policy a request asks for within an
// approval rule's. Each is compared as its own values say its variables
// resolved — for a hand-off, given's for the discobox it is handed to and
// delegated's for the discobox that holds the delegation; for a rule, both
// for the discobox that asked. A variable with no value matches nothing.
//
// It is within when every allow given is covered by an allow of delegated,
// every deny of delegated is given too, and a word both use is defined alike.
// An allow covered but for its conditions is returned as Unsettled, for a
// judge; any other failure is the error, naming the first statement it found.
func Within(given Policy, givenValues map[string]string, delegated Policy, delegatedValues map[string]string) ([]Unsettled, error) {
	for _, word := range sortedKeys(given.Definitions) {
		if theirs, ok := delegated.Definitions[word]; ok && theirs != given.Definitions[word] {
			return nil, fmt.Errorf("%q is defined as %q, and the policy it is held to defines it as %q", word, given.Definitions[word], theirs)
		}
	}
	if err := CarriesDenies(given, givenValues, delegated, delegatedValues); err != nil {
		return nil, err
	}
	allows := bound(given, givenValues, Allow)
	delegatedAllows := bound(delegated, delegatedValues, Allow)
	var unsettled []Unsettled
	for _, allow := range allows {
		var conditional []Statement
		covered := false
		for _, theirs := range delegatedAllows {
			// An allow with no condition at all applies wherever its other
			// parts do, so it is wider than one with any condition, whatever
			// that says. One with a condition of its own is the judge's: an
			// unless can say what a when says.
			if !coversExceptConditions(theirs, allow) || (allow.When == nil && allow.Unless == nil && (theirs.When != nil || theirs.Unless != nil)) {
				continue
			}
			if conditionsWithin(theirs, allow) {
				covered = true
				break
			}
			conditional = append(conditional, theirs)
		}
		switch {
		case covered:
		case len(conditional) > 0:
			unsettled = append(unsettled, Unsettled{Given: allow, Delegated: conditional})
		default:
			return nil, fmt.Errorf("%s: not within any allow of the policy it is held to", Format(allow))
		}
	}
	return unsettled, nil
}

// CarriesDenies reports whether given carries every deny of held, each as its
// own values resolve it: a deny of given that refuses at least what the held
// deny does — the same or more actions, on the same or a wider resource, name
// and scope, and none of a condition the held deny does not have. The error
// names the first deny of held it does not carry.
func CarriesDenies(given Policy, givenValues map[string]string, held Policy, heldValues map[string]string) error {
	givenDenies := bound(given, givenValues, Deny)
	for _, theirs := range bound(held, heldValues, Deny) {
		if !slices.ContainsFunc(givenDenies, func(mine Statement) bool { return denyCarries(mine, theirs) }) {
			return fmt.Errorf("%s: the policy it is held to holds this deny, and it is not carried", Format(theirs))
		}
	}
	return nil
}

// bound is p's statements of one effect, each with its variables replaced by
// their values. A statement a variable of which does not resolve matches
// nothing — an allow allows nothing, a deny denies nothing — and is left out.
func bound(p Policy, values map[string]string, effect Effect) []Statement {
	var out []Statement
	for _, s := range p.Statements {
		if s.Effect != effect {
			continue
		}
		if concrete, ok := boundStatement(s, values); ok {
			out = append(out, concrete)
		}
	}
	return out
}

// denyCarries reports whether mine, a deny, refuses everything theirs does:
// it covers theirs in every part, and applies at least wherever theirs does —
// no when, or theirs, and no unless, or theirs.
func denyCarries(mine, theirs Statement) bool {
	return coversExceptConditions(mine, theirs) && sameCondition(mine.When, theirs.When) && sameCondition(mine.Unless, theirs.Unless)
}

// boundStatement is s with each variable it names replaced by its value, written as a
// literal, or false when one has no value.
func boundStatement(s Statement, values map[string]string) (Statement, bool) {
	ok := true
	name := func(n string) string {
		if !IsVariable(n) {
			return n
		}
		value, found := values[n]
		if !found {
			ok = false
		}
		// A value is a literal: written as a name, it matches itself and
		// nothing else, whatever it holds.
		return escapeGlob(value)
	}
	resource := func(r Resource) Resource {
		if r.Name != nil {
			n := name(*r.Name)
			r.Name = &n
		}
		if r.CreatedBy != "" {
			r.CreatedBy = name(r.CreatedBy)
		}
		return r
	}
	out := s
	if s.Name != nil {
		n := name(*s.Name)
		out.Name = &n
	}
	if s.CreatedBy != "" {
		out.CreatedBy = name(s.CreatedBy)
	}
	out.Within = make([]Resource, len(s.Within))
	for i, r := range s.Within {
		out.Within[i] = resource(r)
	}
	if s.Scope.Name != "" {
		out.Scope.Name = name(s.Scope.Name)
	}
	out.Scope.Within = make([]Resource, len(s.Scope.Within))
	for i, r := range s.Scope.Within {
		out.Scope.Within[i] = resource(r)
	}
	return out, ok
}

// coversExceptConditions reports whether theirs, a delegated allow, covers
// every request mine could, conditions aside: mine's actions are among
// theirs, its type is theirs or theirs is anything, and its name, its
// created-by, its parents and its scope are each theirs or within their glob.
func coversExceptConditions(theirs, mine Statement) bool {
	if !slices.Contains(theirs.Actions, ActionManage) {
		for _, action := range mine.Actions {
			if !slices.Contains(theirs.Actions, action) {
				return false
			}
		}
	}
	if theirs.ResourceType != Anything && theirs.ResourceType != mine.ResourceType {
		return false
	}
	if !nameCovers(theirs.Name, mine.Name) || !createdByCovers(theirs.CreatedBy, mine.CreatedBy) {
		return false
	}
	if !parentsCover(theirs.Within, mine.Within) {
		return false
	}
	// Anywhere is every scope; only a deny says it, so only a deny's
	// covering another is decided here.
	if theirs.Scope.Type == Anywhere {
		return true
	}
	if theirs.Scope.Type != mine.Scope.Type || !nameCovers(&theirs.Scope.Name, &mine.Scope.Name) {
		return false
	}
	return parentsCover(theirs.Scope.Within, mine.Scope.Within)
}

// nameCovers reports whether theirs, a delegated name, covers mine: any name
// covers every name; a name covers itself; a glob covers a literal it matches.
// A glob given is covered only by itself, or by any name.
func nameCovers(theirs, mine *string) bool {
	switch {
	case theirs == nil:
		return true
	case mine == nil:
		return false
	case *theirs == *mine:
		return true
	case isGlob(*mine):
		return false
	}
	matched, err := path.Match(*theirs, *mine)
	return err == nil && matched
}

func createdByCovers(theirs, mine string) bool { return theirs == "" || theirs == mine }

// parentsCover reports whether theirs, a delegated statement's parents,
// cover mine: none cover any, or the same parents, each covered.
func parentsCover(theirs, mine []Resource) bool {
	if len(theirs) == 0 {
		return true
	}
	if len(theirs) != len(mine) {
		return false
	}
	for i := range theirs {
		if theirs[i].Type != mine[i].Type || !nameCovers(theirs[i].Name, mine[i].Name) || !createdByCovers(theirs[i].CreatedBy, mine[i].CreatedBy) {
			return false
		}
	}
	return true
}

// conditionsWithin reports whether code alone can say mine's conditions are
// within theirs: theirs has none, or mine keeps each one theirs has. Mine may
// add a condition where theirs has none, which only narrows it.
func conditionsWithin(theirs, mine Statement) bool {
	return sameCondition(theirs.When, mine.When) && sameCondition(theirs.Unless, mine.Unless)
}

// sameCondition reports whether mine keeps theirs: theirs is none, or the
// same text.
func sameCondition(theirs, mine *string) bool {
	return theirs == nil || (mine != nil && *theirs == *mine)
}

// isGlob reports whether name is a pattern rather than a literal: anything
// path.Match reads as syntax, the escape among it. A name with an escape is
// compared as the pattern it is, so release-\main is not taken for the
// literal a pattern excluding release-main happens to match.
func isGlob(name string) bool { return strings.ContainsAny(name, `*?[\`) }

// escapeGlob is value as a name that matches value alone.
func escapeGlob(value string) string {
	var b strings.Builder
	for _, r := range value {
		if strings.ContainsRune(`*?[\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
