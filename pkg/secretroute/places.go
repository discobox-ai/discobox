package secretroute

import (
	"fmt"
	"slices"
)

// PlacesAllowed reports, as an error naming the first, a place an open route
// of grant names that list allows nowhere it could: a body place no open body
// route of list names, or a message place no open WebSocket route of list
// names at the same pointer in some message both could be (placesOverlap),
// each on a host and a method the grant's route shares. A credential is held
// to every list at once, and a route grants only the kind of use it names,
// so `*` grants no body or message place (REVIEW.md): a grant naming a place
// its secret's or parent's routes never name is refused by the proxy every
// time. Writing one is refused instead, so whoever made it is told rather
// than told it succeeded.
//
// It refuses only what certainly allows nothing: hosts and methods are
// compared for overlap, paths and blocked routes are left to Decide. Routes
// are read as Normalize writes them. An empty list allows nothing at all,
// place or header, and is no concern of this check.
func PlacesAllowed(grant, list []Route) error {
	if len(list) == 0 {
		return nil
	}
	for _, g := range grant {
		if g.Blocked {
			continue
		}
		if g.Body != nil {
			for _, place := range g.Body.Places {
				if !slices.ContainsFunc(list, func(r Route) bool { return placeOverlaps(g, r, place) }) {
					return fmt.Errorf("route %s names the body place %s, which no route there allows on its hosts and methods", Safe(Format(g)), Safe(place))
				}
			}
		}
		if g.WebSocket != nil {
			for _, place := range g.WebSocket.Places {
				if !slices.ContainsFunc(list, func(r Route) bool { return messagePlaceOverlaps(g, r, place) }) {
					return fmt.Errorf("route %s names the message place %s, which no route there allows on its hosts and methods", Safe(Format(g)), Safe(place.Describe()))
				}
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
	return requestsOverlap(g, r)
}

// messagePlaceOverlaps reports whether open route r could allow a message g
// allows with the credential at place.
func messagePlaceOverlaps(g, r Route, place MessagePlace) bool {
	if r.Blocked || r.WebSocket == nil || !slices.ContainsFunc(r.WebSocket.Places, func(p MessagePlace) bool { return placesOverlap(place, p) }) {
		return false
	}
	return requestsOverlap(g, r)
}

// requestsOverlap reports whether two routes share a host and a method.
func requestsOverlap(g, r Route) bool {
	hosts := slices.ContainsFunc(g.Hosts, func(a string) bool {
		return slices.ContainsFunc(r.Hosts, func(b string) bool { return hostContained(a, b) || hostContained(b, a) })
	})
	methods := len(g.Methods) == 0 || len(r.Methods) == 0 || slices.ContainsFunc(g.Methods, func(m string) bool { return slices.Contains(r.Methods, m) })
	return hosts && methods
}
