package targetdb

import (
	"slices"

	"github.com/discobox-ai/discobox/policy"
)

// Lookup is what an entry knows of one statement: the known shapes each of its
// actions has, and the actions that have none.
type Lookup struct {
	// Matches are the statement's actions that have a known shape, in the
	// statement's order, each with every shape that fits it.
	Matches []Match `json:"matches"`
	// Missing are the actions no known shape fits. A statement with any is
	// one whose enforcement the documents must show (§3, §7).
	Missing []string `json:"missing"`
}

// Match is one action of a statement and the known shapes that fit it.
type Match struct {
	Action string  `json:"action"`
	Shapes []Shape `json:"shapes"`
}

// Known reports whether every action of the statement has a known shape.
func (l Lookup) Known() bool { return len(l.Missing) == 0 }

// Lookup finds the known shapes of s. A shape fits an action when the action
// is its own and its resource type, parent types, scope type and the scope's
// parent types are the statement's. Names, conditions, the creator and the
// effect play no part: the rules that recognize a request are the same for
// any of them.
//
// The words that mean more than one thing widen the match in code, with no
// reading: manage is every action on the type, so it fits every shape of the
// type; anything fits every type, and anywhere every scope.
func (e Entry) Lookup(s policy.Statement) Lookup {
	out := Lookup{Matches: []Match{}, Missing: []string{}}
	for _, action := range s.Actions {
		var shapes []Shape
		for _, shape := range e.Statements {
			if (action == policy.ActionManage || shape.Action == action) && fits(shape, s) {
				shapes = append(shapes, shape)
			}
		}
		if len(shapes) == 0 {
			out.Missing = append(out.Missing, action)
			continue
		}
		out.Matches = append(out.Matches, Match{Action: action, Shapes: shapes})
	}
	return out
}

func fits(shape Shape, s policy.Statement) bool {
	if s.ResourceType != policy.Anything {
		if shape.ResourceType != s.ResourceType || !slices.Equal(shape.Within, types(s.Within)) {
			return false
		}
	}
	if s.Scope.Type == policy.Anywhere {
		return true
	}
	return shape.ScopeType == s.Scope.Type && slices.Equal(shape.ScopeWithin, types(s.Scope.Within))
}

func types(resources []policy.Resource) []string {
	out := make([]string, len(resources))
	for i, r := range resources {
		out[i] = r.Type
	}
	return out
}

// For returns the entry that describes host: of the entries that cover it, the
// one whose own host is the longest, so an entry for api.example.com answers
// for it ahead of one for example.com.
func For(entries []Entry, host string) (Entry, bool) {
	host = NormalizeHost(host)
	var best Entry
	found := false
	for _, e := range entries {
		if e.Covers(host) && (!found || len(e.Host) > len(best.Host)) {
			best, found = e, true
		}
	}
	return best, found
}
