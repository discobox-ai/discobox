package secretroute

import (
	"slices"
	"testing"
)

func TestNormalize(t *testing.T) {
	for _, tc := range []struct {
		host, path         string
		wantHost, wantPath string
		wantErr            bool
	}{
		{host: "GitHub.com", wantHost: "github.com"},
		{host: "github.com:443", path: "/", wantHost: "github.com"},
		{host: "github.com", path: "/mcp/", wantHost: "github.com", wantPath: "/mcp"},
		{host: "github.com", path: " /mcp/readonly ", wantHost: "github.com", wantPath: "/mcp/readonly"},
		{host: " ", wantErr: true},
		{host: "github.com/x", wantErr: true},
		{host: "github.com", path: "mcp", wantErr: true},
		{host: "github.com", path: "/mcp?x=1", wantErr: true},
		{host: "github.com", path: "/a//b", wantErr: true},
		{host: "github.com", path: "/a/../b", wantErr: true},
	} {
		host, path, err := Normalize(tc.host, tc.path)
		if (err != nil) != tc.wantErr {
			t.Fatalf("Normalize(%q, %q) error = %v, want error %v", tc.host, tc.path, err, tc.wantErr)
		}
		if host != tc.wantHost || path != tc.wantPath {
			t.Fatalf("Normalize(%q, %q) = %q, %q; want %q, %q", tc.host, tc.path, host, path, tc.wantHost, tc.wantPath)
		}
	}
}

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		text    string
		want    Route
		wantErr bool
	}{
		{text: "githubcopilot.com", want: Route{Host: "githubcopilot.com"}},
		{text: "githubcopilot.com/mcp/readonly", want: Route{Host: "githubcopilot.com", Path: "/mcp/readonly"}},
		{text: "githubcopilot.com/mcp=blocked", want: Route{Host: "githubcopilot.com", Path: "/mcp", Blocked: true}},
		{text: " github.com = blocked ", want: Route{Host: "github.com", Blocked: true}},
		// What a route once carried after its "=" is refused, not read as
		// blocked or dropped: either would change what the person meant.
		{text: "github.com=read-only queries", wantErr: true},
		{text: "=blocked", wantErr: true},
		{text: "", wantErr: true},
	} {
		got, err := Parse(tc.text)
		if (err != nil) != tc.wantErr {
			t.Fatalf("Parse(%q) error = %v, want error %v", tc.text, err, tc.wantErr)
		}
		if got != tc.want {
			t.Fatalf("Parse(%q) = %+v, want %+v", tc.text, got, tc.want)
		}
		if !tc.wantErr {
			if again, err := Parse(Format(got)); err != nil || again != got {
				t.Fatalf("Format did not round-trip %q: %+v, %v", tc.text, again, err)
			}
		}
	}
}

func TestPathCoversWholeSegments(t *testing.T) {
	for _, tc := range []struct {
		prefix, path string
		want         bool
	}{
		{"", "/anything", true},
		{"/mcp", "/mcp", true},
		{"/mcp", "/mcp/", true},
		{"/mcp", "/mcp/readonly", true},
		{"/mcp", "/MCP/readonly", true},
		{"/mcp", "/mcpx", false},
		{"/mcp", "/", false},
		{"/mcp/readonly", "/mcp", false},
	} {
		if got := PathCovers(tc.prefix, tc.path); got != tc.want {
			t.Fatalf("PathCovers(%q, %q) = %v, want %v", tc.prefix, tc.path, got, tc.want)
		}
	}
}

func TestRequestPathsResolveDotSegments(t *testing.T) {
	for raw, want := range map[string][]string{
		"https://x.test":                       {"/"},
		"https://x.test/mcp/readonly?q=1":      {"/mcp/readonly"},
		"https://x.test/mcp/readonly/../write": {"/mcp/write"},
		"https://x.test/mcp/%2e%2e/write":      {"/write"},
		"https://x.test/mcp/readonly/":         {"/mcp/readonly"},
		"/relative/only":                       {"/relative/only"},
		"https://x.test/%zz":                   {"/"},
		// An encoded "/" is read both ways: decoded, and kept in its segment
		// as a server routing on the path as sent would keep it.
		"https://x.test/mcp/x%2F..%2Freadonly":            {"/mcp/readonly", "/mcp/x%2F..%2Freadonly"},
		"https://x.test/mcp/readonly/a%2F..%2F..%2Fwrite": {"/mcp/write", "/mcp/readonly/a%2F..%2F..%2Fwrite"},
	} {
		if got := RequestPaths(raw); !slices.Equal(got, want) {
			t.Fatalf("RequestPaths(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestMatchPicksTheMostSpecificRoute(t *testing.T) {
	routes := []Route{
		{Host: "githubcopilot.com"},
		{Host: "githubcopilot.com", Path: "/mcp", Blocked: true},
		{Host: "githubcopilot.com", Path: "/mcp/readonly"},
		{Host: "github.com", Blocked: true},
		{Host: "api.github.com", Path: "/copilot_internal"},
		{Host: "api.github.com", Path: "/copilot_internal", Blocked: true},
	}
	ids := func(got []Route) []string {
		var out []string
		for _, route := range got {
			out = append(out, Format(route))
		}
		return out
	}
	for _, tc := range []struct {
		host, path string
		want       []string
	}{
		{"api.individual.githubcopilot.com", "/v1/messages", []string{"githubcopilot.com"}},
		{"api.individual.githubcopilot.com", "/mcp", []string{"githubcopilot.com/mcp=blocked"}},
		{"api.individual.githubcopilot.com", "/mcp/readonly", []string{"githubcopilot.com/mcp/readonly"}},
		{"api.individual.githubcopilot.com", "/mcpx", []string{"githubcopilot.com"}},
		// The host's labels decide before the path does: api.github.com's
		// route is closer than github.com's, whatever their paths.
		{"api.github.com", "/copilot_internal/user", []string{"api.github.com/copilot_internal", "api.github.com/copilot_internal=blocked"}},
		{"api.github.com", "/repos/x/y", []string{"github.com=blocked"}},
		{"example.com", "/", nil},
		{"github.com.evil.test", "/", nil},
	} {
		if got := ids(Match(routes, tc.host, tc.path)); !slices.Equal(got, tc.want) {
			t.Fatalf("Match(%s%s) = %v, want %v", tc.host, tc.path, got, tc.want)
		}
	}
}

func TestAHostRouteBeatsAShorterHostWithALongerPath(t *testing.T) {
	routes := []Route{
		{Host: "github.com", Path: "/a/b/c", Blocked: true},
		{Host: "api.github.com"},
	}
	got := Match(routes, "api.github.com", "/a/b/c")
	if len(got) != 1 || got[0].Host != "api.github.com" {
		t.Fatalf("Match = %v, want the api.github.com route", got)
	}
}

func TestDecide(t *testing.T) {
	routes := []Route{
		{Host: "githubcopilot.com"},
		{Host: "githubcopilot.com", Path: "/mcp", Blocked: true},
		{Host: "githubcopilot.com", Path: "/mcp/readonly"},
		{Host: "api.github.com", Path: "/copilot_internal"},
		{Host: "api.github.com", Path: "/copilot_internal", Blocked: true},
	}
	for _, tc := range []struct {
		host, url string
		allowed   bool
	}{
		{"api.individual.githubcopilot.com", "https://api.individual.githubcopilot.com/v1/messages", true},
		{"api.individual.githubcopilot.com", "https://api.individual.githubcopilot.com/mcp", false},
		{"api.individual.githubcopilot.com", "https://api.individual.githubcopilot.com/mcp/readonly/x", true},
		// A path that steps out of the open prefix lands on the blocked one.
		{"api.individual.githubcopilot.com", "https://api.individual.githubcopilot.com/mcp/readonly/../write", false},
		// Read as sent, the encoded slash keeps it inside /mcp/readonly;
		// decoded, it is /mcp, which is blocked. Both readings must pass.
		{"api.individual.githubcopilot.com", "https://api.individual.githubcopilot.com/mcp/readonly%2F..%2F..%2Fmcp", false},
		// A tie with a blocked route refuses.
		{"api.github.com", "https://api.github.com/copilot_internal/user", false},
		{"example.com", "https://example.com/", false},
	} {
		allowed, reason := Decide(routes, tc.host, tc.url)
		if allowed != tc.allowed {
			t.Fatalf("Decide(%s) = %v (%s), want %v", tc.url, allowed, reason, tc.allowed)
		}
		if !allowed && reason == "" {
			t.Fatalf("Decide(%s) refused without a reason", tc.url)
		}
	}
	if allowed, _ := Decide(nil, "example.com", "https://example.com/"); !allowed {
		t.Fatal("a secret with no routes is held to none")
	}
}
