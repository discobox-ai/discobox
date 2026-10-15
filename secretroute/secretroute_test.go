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
		text, host, path, use string
		wantErr               bool
	}{
		{text: "githubcopilot.com", host: "githubcopilot.com"},
		{text: "githubcopilot.com/mcp/readonly", host: "githubcopilot.com", path: "/mcp/readonly"},
		{text: "github.com= read-only queries, a=b ", host: "github.com", use: "read-only queries, a=b"},
		{text: "githubcopilot.com/mcp=GitHub's MCP server", host: "githubcopilot.com", path: "/mcp", use: "GitHub's MCP server"},
		{text: "=a use", wantErr: true},
		{text: "", wantErr: true},
	} {
		host, path, use, err := Parse(tc.text)
		if (err != nil) != tc.wantErr {
			t.Fatalf("Parse(%q) error = %v, want error %v", tc.text, err, tc.wantErr)
		}
		if host != tc.host || path != tc.path || use != tc.use {
			t.Fatalf("Parse(%q) = %q, %q, %q", tc.text, host, path, use)
		}
		if !tc.wantErr {
			if again, _, _, _ := Parse(Format(host, path, use)); again != host {
				t.Fatalf("Format did not round-trip %q", tc.text)
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
		{Host: "githubcopilot.com", Path: "/mcp", UseID: "use_mcp"},
		{Host: "githubcopilot.com", Path: "/mcp/readonly"},
		{Host: "github.com", UseID: "use_gh"},
		{Host: "api.github.com", Path: "/copilot_internal"},
		{Host: "api.github.com", Path: "/copilot_internal", UseID: "use_tie"},
	}
	ids := func(got []Route) []string {
		var out []string
		for _, route := range got {
			out = append(out, route.Host+route.Path+"#"+route.UseID)
		}
		return out
	}
	for _, tc := range []struct {
		host, path string
		want       []string
	}{
		{"api.individual.githubcopilot.com", "/v1/messages", []string{"githubcopilot.com#"}},
		{"api.individual.githubcopilot.com", "/mcp", []string{"githubcopilot.com/mcp#use_mcp"}},
		{"api.individual.githubcopilot.com", "/mcp/readonly", []string{"githubcopilot.com/mcp/readonly#"}},
		{"api.individual.githubcopilot.com", "/mcpx", []string{"githubcopilot.com#"}},
		// The host's labels decide before the path does: api.github.com's
		// route is closer than github.com's, whatever their paths.
		{"api.github.com", "/copilot_internal/user", []string{"api.github.com/copilot_internal#", "api.github.com/copilot_internal#use_tie"}},
		{"api.github.com", "/repos/x/y", []string{"github.com#use_gh"}},
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
		{Host: "github.com", Path: "/a/b/c", UseID: "long"},
		{Host: "api.github.com", UseID: "close"},
	}
	got := Match(routes, "api.github.com", "/a/b/c")
	if len(got) != 1 || got[0].UseID != "close" {
		t.Fatalf("Match = %v, want the api.github.com route", got)
	}
}
