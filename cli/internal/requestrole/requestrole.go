// Package requestrole is how a credential request's policy files are shown to
// the person answering it (ADR 26-11-02-022 §3): grouped by the document they
// are, never by the name the asking discobox gave them, each beside a summary
// of what its routes grant, worked out from the routes the server resolved —
// never from its description, which is the discobox's own words.
//
// It returns plain text. Every name, description, argument and route in it is
// the asking discobox's, so a caller escapes what it draws (cli/REVIEW.md).
package requestrole

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// Policy is one policy file a request part was asked by, as the server
// resolved it.
type Policy struct {
	File        string
	Name        string
	Description string
	Args        map[string]string
	Digest      string
	UnusedArgs  []string
	Routes      []secretroute.Route
}

// Role is every occurrence of one document in a part: the same digest, each
// occurrence with its own arguments.
type Role struct {
	Name        string
	Description string
	Digest      string
	// Args is each occurrence's arguments, in the order they were asked.
	Args []map[string]string
	// Unused is the arguments, across occurrences, that no route path names:
	// they narrow nothing, whatever they suggest.
	Unused []string
	// Routes is every occurrence's routes, in order.
	Routes []secretroute.Route
	// NameShared is set when another role in the part has the same name but
	// is a different document: the name does not say which is which.
	NameShared bool
}

// Group groups a part's policies by document (digest), in the order each
// document first appears.
func Group(policies []Policy) []Role {
	var roles []Role
	index := map[string]int{}
	for _, policy := range policies {
		i, seen := index[policy.Digest]
		if !seen {
			i = len(roles)
			index[policy.Digest] = i
			roles = append(roles, Role{Name: policy.Name, Description: policy.Description, Digest: policy.Digest})
		}
		role := &roles[i]
		role.Args = append(role.Args, policy.Args)
		role.Routes = append(role.Routes, policy.Routes...)
		for _, name := range policy.UnusedArgs {
			if !slices.Contains(role.Unused, name) {
				role.Unused = append(role.Unused, name)
			}
		}
	}
	names := map[string]int{}
	for _, role := range roles {
		names[role.Name]++
	}
	for i := range roles {
		roles[i].NameShared = names[roles[i].Name] > 1
		sort.Strings(roles[i].Unused)
	}
	return roles
}

// ArgsText is the role's occurrences' arguments, one occurrence after
// another: "owner=acme repo=web; owner=acme repo=api". Empty when no
// occurrence has any.
func (r Role) ArgsText() string {
	var occurrences []string
	for _, args := range r.Args {
		var pairs []string
		for _, name := range slices.Sorted(maps.Keys(args)) {
			pairs = append(pairs, name+"="+args[name])
		}
		if len(pairs) > 0 {
			occurrences = append(occurrences, strings.Join(pairs, " "))
		}
	}
	return strings.Join(occurrences, "; ")
}

// Notes are what a reader must not miss about a role, beside its summary.
func (r Role) Notes() []string {
	var notes []string
	if r.NameShared {
		notes = append(notes, fmt.Sprintf("another role here is also named %q but is a different document", r.Name))
	}
	if len(r.Unused) > 0 {
		notes = append(notes, "args not used by any route: "+strings.Join(r.Unused, ", "))
	}
	return notes
}

// Summary is what routes grant, worked out from them alone: the hosts, the
// methods (or any method), the path patterns with every wildcard marked (or
// any path), and the GraphQL operations and MCP tools. On a REST API the path
// is the scope, so it is never left out. Blocked routes are counted, not
// summarized: they take away, and the full list says what.
func Summary(routes []secretroute.Route) string {
	var hosts, methods, paths, graphql, mcp []string
	anyMethod, anyPath, blocked := false, false, 0
	add := func(list *[]string, values ...string) {
		for _, value := range values {
			if !slices.Contains(*list, value) {
				*list = append(*list, value)
			}
		}
	}
	for _, route := range routes {
		if route.Blocked {
			blocked++
			continue
		}
		add(&hosts, route.Hosts...)
		if len(route.Methods) == 0 {
			anyMethod = true
		}
		add(&methods, route.Methods...)
		if len(route.Paths) == 0 {
			anyPath = true
		}
		for _, path := range route.Paths {
			add(&paths, MarkWildcards(path))
		}
		if route.GraphQL != nil {
			ops := strings.Join(route.GraphQL.Operations, ",")
			if ops == "" {
				ops = "any operation"
			}
			if len(route.GraphQL.Fields) > 0 {
				ops += ":" + strings.Join(route.GraphQL.Fields, ",")
			}
			add(&graphql, ops)
		}
		if route.MCP != nil {
			if len(route.MCP.Tools) == 0 {
				add(&mcp, "any tool")
			}
			add(&mcp, route.MCP.Tools...)
		}
	}
	if len(hosts) == 0 {
		if blocked > 0 {
			return fmt.Sprintf("nothing open; %d blocked", blocked)
		}
		return "nothing"
	}
	methodText := strings.Join(methods, ",")
	if anyMethod {
		methodText = "any method"
		if len(methods) > 0 {
			methodText += " (and " + strings.Join(methods, ",") + ")"
		}
	}
	pathText := strings.Join(paths, ", ")
	if anyPath {
		pathText = "any path"
		if len(paths) > 0 {
			pathText += " (and " + strings.Join(paths, ", ") + ")"
		}
	}
	parts := []string{strings.Join(hosts, ","), methodText, pathText}
	if len(graphql) > 0 {
		parts = append(parts, "graphql "+strings.Join(graphql, "; "))
	}
	if len(mcp) > 0 {
		parts = append(parts, "mcp "+strings.Join(mcp, ","))
	}
	if blocked > 0 {
		parts = append(parts, fmt.Sprintf("%d blocked", blocked))
	}
	return strings.Join(parts, " · ")
}

// MarkWildcards brackets every path segment holding a wildcard, so a `**`
// that opens a whole API is seen, not read past: /repos/acme/[**].
func MarkWildcards(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if strings.Contains(segment, "*") {
			segments[i] = "[" + segment + "]"
		}
	}
	return strings.Join(segments, "/")
}
