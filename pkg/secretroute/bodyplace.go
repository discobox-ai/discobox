package secretroute

import (
	"fmt"
	"slices"
)

// BodyPlacesAllowed reports, as an error naming the first, a body place an
// open route of grant names that list allows nowhere it could: no open route
// of list with a body block names that place on a host and a method the
// grant's route shares. A credential is held to every list at once, and a
// route grants only the kind of use it names, so `*` grants no body place
// (REVIEW.md): a grant naming a place its secret's or parent's routes never
// name is refused by the proxy every time. Writing one is refused instead,
// so whoever made it is told rather than told it succeeded.
//
// It refuses only what certainly allows nothing: hosts and methods are
// compared for overlap, paths and blocked routes are left to Decide. Routes
// are read as Normalize writes them. An empty list allows nothing at all,
// body place or header, and is no concern of this check.
func BodyPlacesAllowed(grant, list []Route) error {
	if len(list) == 0 {
		return nil
	}
	for _, g := range grant {
		if g.Blocked || g.Body == nil {
			continue
		}
		for _, place := range g.Body.Places {
			if !slices.ContainsFunc(list, func(r Route) bool { return placeOverlaps(g, r, place) }) {
				return fmt.Errorf("route %s names the body place %s, which no route there allows on its hosts and methods", safe(Format(g)), safe(place))
			}
		}
	}
	return nil
}

// placeOverlaps reports whether open route r could allow a request g allows
// with the credential at place.
func placeOverlaps(g, r Route, place string) bool {
	if r.Blocked || r.Body == nil || !slices.Contains(r.Body.Places, place) {
		return false
	}
	hosts := slices.ContainsFunc(g.Hosts, func(a string) bool {
		return slices.ContainsFunc(r.Hosts, func(b string) bool { return hostContained(a, b) || hostContained(b, a) })
	})
	methods := len(g.Methods) == 0 || len(r.Methods) == 0 || slices.ContainsFunc(g.Methods, func(m string) bool { return slices.Contains(r.Methods, m) })
	return hosts && methods
}
