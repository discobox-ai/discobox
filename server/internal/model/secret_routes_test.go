package model

import "testing"

func TestReplaceRoutes(t *testing.T) {
	var sec Secret
	changed, err := sec.ReplaceRoutes([]SecretRoute{
		{Host: "GitHubCopilot.com", Path: "/"},
		{Host: "githubcopilot.com", Path: "/mcp/", Use: " read-only MCP "},
		{Host: "githubcopilot.com", Path: "/mcp", Use: "read-only MCP"},
		{Host: "github.com", Use: "reading GitHub"},
	})
	if err != nil || !changed {
		t.Fatalf("ReplaceRoutes() = %v, %v; want a change", changed, err)
	}
	if len(sec.Routes) != 3 {
		t.Fatalf("routes = %+v, want the repeat dropped", sec.Routes)
	}
	if got := sec.Routes[0]; got != (SecretRoute{Host: "githubcopilot.com"}) {
		t.Fatalf("route = %+v, want the whole host with no use ID", got)
	}
	if got := sec.Routes[1]; got.Path != "/mcp" || got.Use != "read-only MCP" || got.UseID == "" {
		t.Fatalf("route = %+v, want it normalized with a minted use ID", got)
	}
	minted := sec.Routes[1].UseID

	changed, err = sec.ReplaceRoutes([]SecretRoute{
		{Host: "githubcopilot.com", Path: "/mcp", Use: "read-only MCP", UseID: "use_forged"},
		{Host: "github.com", Use: "reading GitHub, changed"},
	})
	if err != nil || !changed {
		t.Fatalf("ReplaceRoutes() = %v, %v; want a change", changed, err)
	}
	if sec.Routes[0].UseID != minted {
		t.Fatalf("use ID = %q, want the unchanged route's %q kept and a given one ignored", sec.Routes[0].UseID, minted)
	}

	same := append([]SecretRoute(nil), sec.Routes...)
	if changed, err = sec.ReplaceRoutes(same); err != nil || changed {
		t.Fatalf("ReplaceRoutes(same) = %v, %v; want no change", changed, err)
	}

	for _, bad := range []SecretRoute{{Host: " "}, {Host: "github.com", Path: "mcp"}} {
		if _, err := sec.ReplaceRoutes([]SecretRoute{bad}); err == nil {
			t.Fatalf("ReplaceRoutes(%+v) succeeded, want it refused", bad)
		}
	}
	if len(sec.ProxyRoutes()) != 2 || sec.ProxyRoutes()[0].UseID != minted {
		t.Fatalf("proxy routes = %+v, want them by use ID", sec.ProxyRoutes())
	}
}
