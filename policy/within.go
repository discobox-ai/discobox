package policy

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// Unsettled is an allow handed on that some delegated allows cover in every
// part but their conditions: whether its conditions are within one of
// theirs is a judge's to say (ADR 26-10-20-782 §11). It is within if they are
// within any one.
type Unsettled struct {
	Given     Statement
	Delegated []Statement
}

// Within reports whether given, a policy handed on, is within delegated, the
// policy of the delegation it is handed on under (ADR 26-10-20-782 §11). Each
// is compared as its own values say its variables resolved: given's for the
// discobox it is handed to, delegated's for the discobox that holds the
// delegation. A variable with no value matches nothing.
//
// It is within when every allow given is covered by a delegated allow, every
// delegated deny is given too, and a word both use is defined alike. An allow
// covered but for its conditions is returned as Unsettled, for a judge; any
// other failure is the error, naming the first statement it found.
func Within(given Policy, givenValues map[string]string, delegated Policy, delegatedValues map[string]string) ([]Unsettled, error) {
	for _, word := range sortedKeys(given.Definitions) {
		if theirs, ok := delegated.Definitions[word]; ok && theirs != given.Definitions[word] {
			return nil, fmt.Errorf("%q is defined as %q, and the delegation defines it as %q", word, given.Definitions[word], theirs)
		}
	}
	var allows, givenDenies []Statement
	for _, s := range given.Statements {
		concrete, ok := bound(s, givenValues)
		switch {
		case !ok:
			// A statement a variable of which does not resolve matches
			// nothing: an allow allows nothing, a deny denies nothing.
		case s.Effect == Allow:
			allows = append(allows, concrete)
		default:
			givenDenies = append(givenDenies, concrete)
		}
	}
	var delegatedAllows []Statement
	for _, s := range delegated.Statements {
		concrete, ok := bound(s, delegatedValues)
		if !ok {
			continue
		}
		if s.Effect == Allow {
			delegatedAllows = append(delegatedAllows, concrete)
			continue
		}
		if !slices.ContainsFunc(givenDenies, func(d Statement) bool { return sameStatement(d, concrete) }) {
			return nil, fmt.Errorf("%s: the delegation holds this deny, and it is not carried", Format(concrete))
		}
	}
	var unsettled []Unsettled
	for _, allow := range allows {
		var conditional []Statement
		covered := false
		for _, theirs := range delegatedAllows {
			if !coversExceptConditions(theirs, allow) {
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
			return nil, fmt.Errorf("%s: not within any allow the delegation holds", Format(allow))
		}
	}
	return unsettled, nil
}

// bound is s with each variable it names replaced by its value, written as a
// literal, or false when one has no value.
func bound(s Statement, values map[string]string) (Statement, bool) {
	ok := true
	name := func(n string) string {
		if !IsVariable(n) {
			return n
		}
		value, found := values[n]
		if !found {
			ok = false
		}
		return value
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

// sameStatement reports whether two statements say the same, actions in any
// order.
func sameStatement(a, b Statement) bool {
	a.Actions, b.Actions = slices.Sorted(slices.Values(a.Actions)), slices.Sorted(slices.Values(b.Actions))
	return Format(a) == Format(b)
}

func isGlob(name string) bool { return strings.ContainsAny(name, "*?[") }
