package secretroute

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestProveContained(t *testing.T) {
	routes := func(texts []string) []Route {
		var out []Route
		for _, s := range texts {
			r, e := Parse(s)
			if e != nil {
				t.Fatal(e)
			}
			out = append(out, r)
		}
		return out
	}
	for _, tc := range []struct {
		name         string
		grant, limit []string
		ok           bool
	}{
		{"same policy with exception", []string{"api.github.com/repos/acme/**", "DELETE api.github.com/repos/acme/web=blocked"}, []string{"DELETE api.github.com/repos/acme/web=blocked", "api.github.com/repos/acme/**"}, true},
		{"removed exception", []string{"api.github.com/repos/acme/**"}, []string{"api.github.com/repos/acme/**", "DELETE api.github.com/repos/acme/web=blocked"}, false},
		{"narrow repository", []string{"GET api.github.com/repos/acme/web/**"}, []string{"GET,POST *.github.com/repos/acme/**"}, true},
		{"wide host", []string{"GET *.github.com/repos/acme/web/**"}, []string{"GET api.github.com/repos/acme/**"}, false},
		{"wide method", []string{"api.github.com/repos/acme/**"}, []string{"GET api.github.com/repos/acme/**"}, false},
		{"wide path", []string{"GET api.github.com/repos/**"}, []string{"GET api.github.com/repos/acme/**"}, false},
		{"graphql narrowing", []string{"POST api.github.com/graphql graphql=query:repository"}, []string{"POST api.github.com/graphql graphql=query:repo*"}, true},
		{"graphql widening", []string{"POST api.github.com/graphql graphql=mutation"}, []string{"POST api.github.com/graphql graphql=query"}, false},
		{"mcp narrowing", []string{"tools.example/mcp mcp=read_file"}, []string{"tools.example/mcp mcp=read_*"}, true},
		{"mcp widening", []string{"tools.example/mcp mcp=*"}, []string{"tools.example/mcp mcp=read_*"}, false},
		{"deny only", []string{"*=blocked"}, []string{"api.github.com"}, true},
		{"exception changed", []string{"api.github.com", "DELETE api.github.com/other=blocked"}, []string{"api.github.com", "DELETE api.github.com/repos/acme/web=blocked"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ProveContained(routes([]string{"*"}), routes(tc.grant), routes(tc.limit))
			if (err == nil) != tc.ok {
				t.Fatalf("proof = %v, want %v", err, tc.ok)
			}
		})
	}
}

// A delegation can combine up to 32 policy documents. Repeated shared routes
// and each role's paths must not make an otherwise identical member unprovable.
func TestProveContainedPolicyUnion(t *testing.T) {
	var delegation []Route
	var member []Route
	for repo := range 8 {
		for role := range 2 {
			var policy []Route
			for route := range 10 {
				policy = append(policy, Route{Hosts: []string{"api.github.com"}, Methods: []string{"GET"}, Paths: []string{fmt.Sprintf("/repos/acme/repo%d/role%d/route%d/**", repo, role, route)}})
			}
			delegation = append(delegation, policy...)
			if repo == 0 && role == 0 {
				member = policy
			}
		}
	}
	if err := ProveContained([]Route{{Hosts: []string{"api.github.com"}}}, member, delegation); err != nil {
		t.Fatal(err)
	}
}

func TestProveContainedBudgets(t *testing.T) {
	secret := []Route{{Hosts: []string{"api.github.com"}}}
	for _, tc := range []struct {
		name         string
		grant, limit []Route
		want         string
	}{
		{"route count", make([]Route, 513), nil, "grant routes: too many routes"},
		{"bytes", []Route{{Hosts: []string{"api.github.com"}, Paths: []string{"/" + strings.Repeat("x", 256<<10)}}}, nil, "grant routes: route containment proof input"},
		{"patterns", []Route{{Hosts: []string{"api.github.com"}, Methods: slices.Repeat([]string{"GET"}, 16384)}}, nil, "grant routes: route containment proof input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ProveContained(secret, tc.grant, tc.limit); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("proof=%v, want %s", err, tc.want)
			}
		})
	}
	var grant, limit []Route
	for i := range 512 {
		grant = append(grant, Route{Hosts: []string{"api.github.com"}, Methods: []string{"GET"}, Paths: []string{fmt.Sprintf("/child%d", i)}})
		limit = append(limit, Route{Hosts: []string{"api.github.com"}, Methods: []string{"GET"}, Paths: []string{fmt.Sprintf("/parent%d", i)}})
	}
	if err := ProveContained(secret, grant, limit); err == nil || !strings.Contains(err.Error(), "comparison limit") {
		t.Fatalf("comparison budget=%v", err)
	}
	// Large identical policies need no pairwise search.
	if err := ProveContained(secret, grant, grant); err != nil {
		t.Fatal(err)
	}
}

func TestProveContainedBudgetsLongPaths(t *testing.T) {
	grant := []Route{{Hosts: []string{"api.github.com"}, Paths: []string{strings.Repeat("/x", 120000)}}}
	var limit []Route
	for i := range 512 {
		limit = append(limit, Route{Hosts: []string{"api.github.com"}, Paths: []string{fmt.Sprintf("/parent%d", i)}})
	}
	if err := ProveContained([]Route{{Hosts: []string{"api.github.com"}}}, grant, limit); err == nil || !strings.Contains(err.Error(), "comparison limit") {
		t.Fatalf("long-path budget=%v", err)
	}
}
