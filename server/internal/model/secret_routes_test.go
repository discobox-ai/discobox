package model

import (
	"testing"

	"github.com/discobox-ai/discobox/pkg/secretroute"
)

func TestReplaceRoutes(t *testing.T) {
	var sec Secret
	changed, err := sec.ReplaceRoutes([]SecretRoute{
		{Host: "GitHubCopilot.com", Path: "/"},
		{Host: "githubcopilot.com", Path: "/mcp/", Blocked: true},
		{Host: "githubcopilot.com", Path: "/mcp", Blocked: true},
		{Host: "github.com", Blocked: true},
	})
	if err != nil || !changed {
		t.Fatalf("ReplaceRoutes() = %v, %v; want a change", changed, err)
	}
	if len(sec.Routes) != 3 {
		t.Fatalf("routes = %+v, want the repeat dropped", sec.Routes)
	}
	if got := sec.Routes[0]; got != (SecretRoute{Host: "githubcopilot.com"}) {
		t.Fatalf("route = %+v, want the whole host", got)
	}
	if got := sec.Routes[1]; got != (SecretRoute{Host: "githubcopilot.com", Path: "/mcp", Blocked: true}) {
		t.Fatalf("route = %+v, want it normalized and still blocked", got)
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

	for _, bad := range []SecretRoute{{Host: " "}, {Host: "github.com", Path: "mcp"}} {
		if _, err := sec.ReplaceRoutes([]SecretRoute{bad}); err == nil {
			t.Fatalf("ReplaceRoutes(%+v) succeeded, want it refused", bad)
		}
	}
	sec.Routes = []SecretRoute{{Host: "githubcopilot.com"}, {Host: "github.com", Blocked: true}}
	want := []secretroute.Route{{Host: "githubcopilot.com"}, {Host: "github.com", Blocked: true}}
	got := sec.ProxyRoutes()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("proxy routes = %+v, want %+v", got, want)
	}
}
