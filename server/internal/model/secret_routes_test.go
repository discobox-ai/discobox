package model

import (
	"reflect"
	"testing"
)

func TestReplaceRoutes(t *testing.T) {
	var sec Secret
	changed, err := sec.ReplaceRoutes([]SecretRoute{
		{Hosts: []string{"GitHubCopilot.com"}},
		{Hosts: []string{"githubcopilot.com"}, Paths: []string{"/mcp/"}, Blocked: true},
		{Hosts: []string{"githubcopilot.com"}, Paths: []string{"/mcp"}, Blocked: true},
		{Hosts: []string{"github.com", "*.github.com"}, Methods: []string{"post", "GET"}, Blocked: true},
	})
	if err != nil || !changed {
		t.Fatalf("ReplaceRoutes() = %v, %v; want a change", changed, err)
	}
	want := []SecretRoute{
		{Hosts: []string{"githubcopilot.com"}},
		{Hosts: []string{"githubcopilot.com"}, Paths: []string{"/mcp"}, Blocked: true},
		{Hosts: []string{"*.github.com", "github.com"}, Methods: []string{"GET", "POST"}, Blocked: true},
	}
	if !reflect.DeepEqual(sec.Routes, want) {
		t.Fatalf("routes = %+v, want %+v: normalized, in order, the repeat dropped", sec.Routes, want)
	}

	same := append([]SecretRoute(nil), sec.Routes...)
	if changed, err = sec.ReplaceRoutes(same); err != nil || changed {
		t.Fatalf("ReplaceRoutes(same) = %v, %v; want no change", changed, err)
	}
	// Unblocking a route is a change.
	same[1].Blocked = false
	if changed, err = sec.ReplaceRoutes(same); err != nil || !changed {
		t.Fatalf("ReplaceRoutes(unblocked) = %v, %v; want a change", changed, err)
	}

	for _, bad := range []SecretRoute{{Hosts: []string{" "}}, {Hosts: []string{"github.com"}, Paths: []string{"mcp"}}, {}} {
		if _, err := sec.ReplaceRoutes([]SecretRoute{bad}); err == nil {
			t.Fatalf("ReplaceRoutes(%+v) succeeded, want it refused", bad)
		}
	}
}

func TestRouteSetsAndHosts(t *testing.T) {
	a, err := NormalizeRoutes([]SecretRoute{{Hosts: []string{"a.example"}}, {Hosts: []string{"b.example"}, Paths: []string{"/x/**"}}})
	if err != nil {
		t.Fatal(err)
	}
	b := []SecretRoute{a[1], a[0]}
	if !SameRouteSet(a, b) || SameRoutes(a, b) {
		t.Fatalf("SameRouteSet = %v, SameRoutes = %v; want the same set in another order", SameRouteSet(a, b), SameRoutes(a, b))
	}
	if SameRouteSet(a, a[:1]) {
		t.Fatal("SameRouteSet of a list and part of it = true")
	}
	routes := []SecretRoute{
		{Hosts: []string{"*.github.com", "github.com"}},
		{Hosts: []string{"github.com"}, Paths: []string{"/x"}},
		{Hosts: []string{"gist.github.com"}, Blocked: true},
	}
	if got, want := OpenRouteHosts(routes), []string{"*.github.com", "github.com"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("OpenRouteHosts = %q, want %q", got, want)
	}
}
