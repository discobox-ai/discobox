package secretroute

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// ProveContained proves that every request allowed by grant under secret is
// allowed by limit under secret. It is deliberately incomplete: identical
// normalized lists (including exceptions) and restrictions of open-only lists
// are supported. Other expressions fail closed, rather than using samples.
// Work is bounded to 128 routes and 4096 list comparisons.
func ProveContained(secret, grant, limit []Route) error {
	normalize := func(in []Route) ([]Route, error) {
		if len(in) > 128 {
			return nil, fmt.Errorf("too many routes for containment proof")
		}
		out := make([]Route, 0, len(in))
		bytes, patterns := 0, 0
		for _, r := range in {
			bytes += len(Format(r))
			patterns += len(r.Hosts) + len(r.Methods) + len(r.Paths)
			if r.GraphQL != nil {
				patterns += len(r.GraphQL.Operations) + len(r.GraphQL.Fields)
			}
			if r.MCP != nil {
				patterns += len(r.MCP.Tools)
			}
			if bytes > 65536 || patterns > 256 {
				return nil, fmt.Errorf("route containment proof input exceeds its limit")
			}
			n, err := Normalize(r)
			if err != nil {
				return nil, err
			}
			out = append(out, n)
		}
		slices.SortFunc(out, func(a, b Route) int { return strings.Compare(Format(a), Format(b)) })
		return slices.CompactFunc(out, func(a, b Route) bool { return reflect.DeepEqual(a, b) }), nil
	}
	s, err := normalize(secret)
	if err != nil {
		return err
	}
	g, err := normalize(grant)
	if err != nil {
		return err
	}
	d, err := normalize(limit)
	if err != nil {
		return err
	}
	// An absent binding list inherits. Both effective grants include secret.
	if len(g) == 0 {
		g = s
	}
	if len(d) == 0 {
		d = s
	}
	if reflect.DeepEqual(g, d) || reflect.DeepEqual(s, d) {
		return nil
	}
	if len(g)*len(d) > 4096 {
		return fmt.Errorf("route containment proof exceeds its comparison limit")
	}
	// Blocks participate in specificity selection and match unknown bodies.
	// Never reason about them as independent negative rectangles.
	for _, r := range d {
		if r.Blocked {
			return fmt.Errorf("containment with changed blocked-route lists cannot be proved")
		}
	}
	for _, r := range g {
		if r.Blocked {
			continue
		}
		fits := false
		for _, parent := range d {
			if openContained(r, parent) {
				fits = true
				break
			}
		}
		if !fits {
			return fmt.Errorf("route containment cannot be proved; use a provably narrower list or ask a person")
		}
	}
	return nil
}

func openContained(g, d Route) bool {
	if !patternsContained(g.Hosts, d.Hosts, hostContained) || !patternsContained(g.Methods, d.Methods, func(a, b string) bool { return a == b }) || !patternsContained(g.Paths, d.Paths, pathContained) {
		return false
	}
	if d.GraphQL != nil {
		if g.GraphQL == nil || !patternsContained(g.GraphQL.Operations, d.GraphQL.Operations, func(a, b string) bool { return a == b }) || !patternsContained(g.GraphQL.Fields, d.GraphQL.Fields, nameContained) {
			return false
		}
	}
	if d.MCP != nil {
		if g.MCP == nil || !patternsContained(g.MCP.Tools, d.MCP.Tools, nameContained) {
			return false
		}
	}
	return true
}

func patternsContained(g, d []string, contains func(string, string) bool) bool {
	if len(d) == 0 {
		return true
	}
	if len(g) == 0 {
		return false
	}
	for _, a := range g {
		if !slices.ContainsFunc(d, func(b string) bool { return contains(a, b) }) {
			return false
		}
	}
	return true
}
func hostContained(g, d string) bool {
	if d == "*" || g == d {
		return true
	}
	if strings.HasPrefix(d, "*.") {
		return strings.HasSuffix(g, strings.TrimPrefix(d, "*"))
	}
	return false
}
func nameContained(g, d string) bool {
	return g == d || strings.HasSuffix(d, "*") && strings.HasPrefix(g, strings.TrimSuffix(d, "*"))
}
func pathContained(g, d string) bool {
	if g == d {
		return true
	}
	gs, ds := splitPath(g), splitPath(d)
	for i, p := range ds {
		if p == "**" {
			return true
		}
		if i >= len(gs) || gs[i] == "**" || p != "*" && p != gs[i] {
			return false
		}
	}
	return len(gs) == len(ds)
}
