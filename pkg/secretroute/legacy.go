package secretroute

import (
	"fmt"
	"slices"
	"strings"
)

// LegacyRoute is a route as it was stored before routes named host patterns:
// one host, which covered itself and every host beneath it, an optional path
// prefix on whole segments, and whether it was blocked. A caller whose legacy
// routes carried a use or a judge folds that into Blocked (ADR 26-10-27-880
// §8: a judged route becomes a blocked one).
type LegacyRoute struct {
	Host    string
	Path    string
	Blocked bool
}

// RefusingList is the route list that refuses every request: a blocked `*`
// and nothing open. A list cannot be left empty to say it, since an empty
// binding list inherits its secret's.
func RefusingList() []Route {
	return []Route{{Hosts: []string{AnyHost}, Blocked: true}}
}

// FromLegacyList is the one reading of a legacy route list (ADR 26-10-27-880
// §8), shared by the control plane's migration and the pool's old persisted
// routes, so a stored list reads the same on either side. legacy are the
// list's routes in the legacy shape and current those already in the new one
// on the same list (a row both shapes were written to; nil for none), which
// are kept as they are. Every legacy route FromLegacy converts is converted
// exactly; any other is read by fromLegacyClosed, which never widens.
//
// The result is RefusingList when a blocked legacy route's host does not
// read, since nothing narrower is known to cover what it refused; otherwise
// current followed by the converted legacy routes, repeats collapsed. It is
// RefusingList, too, when that comes to no routes from a list that had some:
// an emptied list must never read as no routes, which a binding takes as its
// secret's. A list with no routes at all stays empty. Notes are sentences,
// one for each legacy route not converted exactly, for the caller to log
// beside what the list belongs to.
func FromLegacyList(legacy []LegacyRoute, current []Route) (converted []Route, notes []string) {
	for _, route := range current {
		converted = appendNew(converted, route)
	}
	for _, old := range legacy {
		route, err := FromLegacy(old.Host, old.Path, old.Blocked)
		if err == nil {
			converted = appendNew(converted, route)
			continue
		}
		next, ok, refuseList := fromLegacyClosed(old.Host, old.Path, old.Blocked)
		switch {
		case refuseList:
			notes = append(notes, fmt.Sprintf("the list refuses every request now: blocked route %s does not read (%v), and nothing narrower is known to cover what it refused", legacyText(old), err))
			return RefusingList(), notes
		case !ok:
			notes = append(notes, fmt.Sprintf("dropped open route %s: %v; nothing a route says now allows exactly what it did, and dropping it allows less, never more", legacyText(old), err))
		default:
			notes = append(notes, fmt.Sprintf("blocked route %s became %s: %v; it refuses at least what it did", legacyText(old), Format(next), err))
			converted = appendNew(converted, next)
		}
	}
	if len(legacy)+len(current) > 0 && len(converted) == 0 {
		notes = append(notes, "the list refuses every request now: none of its routes converts, and an empty list would read as no routes")
		return RefusingList(), notes
	}
	return converted, notes
}

// appendNew appends a route unless the list holds it already.
func appendNew(list []Route, route Route) []Route {
	text := Format(route)
	if slices.ContainsFunc(list, func(r Route) bool { return Format(r) == text }) {
		return list
	}
	return append(list, route)
}

// legacyText is a legacy route as a person typed it.
func legacyText(route LegacyRoute) string {
	text := fmt.Sprintf("%q", route.Host+route.Path)
	if route.Blocked {
		text += BlockedSuffix
	}
	return text
}

// fromLegacyClosed reads a legacy route whatever it holds, never widening
// what its list allowed (ADR 26-10-27-880 §8): it is FromLegacy for a route
// FromLegacy converts, and for one it refuses
//
//   - an open route is left out (ok false): it only ever allowed, so its list
//     allows less without it, never more;
//   - a blocked route blocks its path cut back to the last segment before the
//     first one no path can spell (`*`, which was literal and would now be a
//     glob, or `=`): /repos/acme/we*b/x blocks /repos/acme/**. That covers all
//     the legacy prefix did, and is at least as specific as every open route
//     of its list that covered any of it — one reaching past the cut holds
//     that segment too and is left out — and a blocked route wins a tie, so it
//     refuses at least what it did;
//   - a blocked route whose host does not read refuses its whole list
//     (refuseList): nothing narrower is known to cover what it refused.
//
// FromLegacyList applies it to a whole list.
func fromLegacyClosed(host, path string, blocked bool) (route Route, ok, refuseList bool) {
	route, err := FromLegacy(host, path, blocked)
	if err == nil {
		return route, true, false
	}
	if !blocked {
		return Route{}, false, false
	}
	if route, err = FromLegacy(host, "", true); err != nil {
		return Route{}, false, true
	}
	segments := strings.Split(strings.Trim(strings.TrimSpace(path), "/"), "/")
	cut := ""
	for _, segment := range segments {
		if _, err := FromLegacy(host, cut+"/"+segment, true); err != nil {
			break
		}
		cut += "/" + segment
	}
	if cut == "" {
		return route, true, false
	}
	route, err = FromLegacy(host, cut, true)
	return route, err == nil, err != nil
}
