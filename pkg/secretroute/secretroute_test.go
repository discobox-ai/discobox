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
		allowed, reason := Decide([][]Route{routes}, tc.host, tc.url)
		if allowed != tc.allowed {
			t.Fatalf("Decide(%s) = %v (%s), want %v", tc.url, allowed, reason, tc.allowed)
		}
		if !allowed && reason == "" {
			t.Fatalf("Decide(%s) refused without a reason", tc.url)
		}
	}
}

// A credential is sent where its routes say and only there: no routes, or no
// lists at all, allows nothing (ADR 26-10-27-880 §1).
func TestDecideAllowsNothingWithoutRoutes(t *testing.T) {
	for _, lists := range [][][]Route{nil, {}, {nil}, {{{Host: "example.com"}}, {}}} {
		if allowed, reason := Decide(lists, "example.com", "https://example.com/"); allowed || reason == "" {
			t.Fatalf("Decide(%v) = %v %q, want a refusal", lists, allowed, reason)
		}
	}
}

// Every list must allow: a binding narrows its secret, and a parent narrows
// the binding handed on under it. No list unblocks what another blocks.
func TestDecideHoldsARequestToEveryList(t *testing.T) {
	secret := []Route{{Host: "github.com"}, {Host: "github.com", Path: "/admin", Blocked: true}}
	binding := []Route{{Host: "api.github.com", Path: "/repos/acme"}}
	parent := []Route{{Host: AnyHost}}
	lists := [][]Route{secret, binding, parent}
	for _, tc := range []struct {
		host, url string
		allowed   bool
	}{
		{"api.github.com", "https://api.github.com/repos/acme/web/pulls", true},
		{"api.github.com", "https://api.github.com/repos/other/web", false}, // off the binding
		{"github.com", "https://github.com/admin", false},                   // off the binding, and blocked by the secret
	} {
		if allowed, reason := Decide(lists, tc.host, tc.url); allowed != tc.allowed {
			t.Fatalf("Decide(%s) = %v (%s), want %v", tc.url, allowed, reason, tc.allowed)
		}
	}
	// A binding's open route cannot reopen a path its secret blocks.
	reopen := [][]Route{secret, {{Host: "github.com", Path: "/admin"}}}
	if allowed, _ := Decide(reopen, "github.com", "https://github.com/admin/x"); allowed {
		t.Fatal("a binding reopened a path its secret blocks")
	}
}

// AnyHost covers every host, and is the least specific route: a route naming
// a host answers for what it covers, open or blocked.
func TestAnyHostIsTheLeastSpecificRoute(t *testing.T) {
	routes := []Route{{Host: AnyHost}, {Host: "internal.example", Blocked: true}}
	if allowed, _ := Decide([][]Route{routes}, "registry.npmjs.org", "https://registry.npmjs.org/x"); !allowed {
		t.Fatal("* did not cover an unnamed host")
	}
	if allowed, _ := Decide([][]Route{routes}, "ci.internal.example", "https://ci.internal.example/"); allowed {
		t.Fatal("a named blocked route lost to *")
	}
	got, err := Parse("*/v1")
	if err != nil || got != (Route{Host: AnyHost, Path: "/v1"}) {
		t.Fatalf("Parse(*/v1) = %+v, %v", got, err)
	}
	for _, bad := range []string{"*.github.com", "api.*.com"} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("Parse(%q) accepted a wildcard inside a host", bad)
		}
	}
}

func TestHostAllowed(t *testing.T) {
	lists := [][]Route{
		{{Host: "github.com"}, {Host: "github.com", Path: "/admin", Blocked: true}},
		{{Host: "api.github.com"}},
	}
	if !HostAllowed(lists, "api.github.com") {
		t.Fatal("a host every list opens was refused")
	}
	if HostAllowed(lists, "uploads.github.com") {
		t.Fatal("a host one list does not open was allowed")
	}
	if HostAllowed(nil, "api.github.com") || HostAllowed([][]Route{{}}, "api.github.com") {
		t.Fatal("no routes allowed a host")
	}
	if HostAllowed([][]Route{{{Host: "api.github.com", Blocked: true}}}, "api.github.com") {
		t.Fatal("a host only a blocked route covers was allowed")
	}
	// A host-wide blocked route shuts its host against a wider open one, as
	// Decide refuses every path there; a blocked path leaves the rest open.
	shut := [][]Route{{{Host: "github.com"}, {Host: "uploads.github.com", Blocked: true}}}
	if HostAllowed(shut, "uploads.github.com") {
		t.Fatal("a host its own blocked route shuts was allowed")
	}
	if !HostAllowed(shut, "api.github.com") {
		t.Fatal("a sibling host the blocked route does not cover was refused")
	}
	partly := [][]Route{{{Host: "github.com"}, {Host: "uploads.github.com", Path: "/x", Blocked: true}}}
	if !HostAllowed(partly, "uploads.github.com") {
		t.Fatal("a host with only a path blocked was refused")
	}
	// An open path on a host-wide blocked host still opens that path, as
	// Decide allows it, so the value is handed out for it.
	path := [][]Route{{{Host: "github.com", Blocked: true}, {Host: "github.com", Path: "/acme"}}}
	if !HostAllowed(path, "github.com") {
		t.Fatal("a host whose open path outranks its block was refused")
	}
	if allowed, _ := Decide(path, "github.com", "https://github.com/acme/web"); !allowed {
		t.Fatal("Decide refused the open path HostAllowed admitted")
	}
	reopened := [][]Route{{{Host: AnyHost, Blocked: true}, {Host: "api.github.com"}}}
	if !HostAllowed(reopened, "api.github.com") || HostAllowed(reopened, "example.com") {
		t.Fatal("a named open route under a blocked * was not read as the most specific")
	}
}

// A route is shown to whoever approves or reads it, so it may not carry a
// control character that would move their terminal.
func TestARouteRefusesControlCharacters(t *testing.T) {
	for _, bad := range []string{"evil\x1b]0;x\x07.com", "github.com/a\x1b[2Jb", "github.com/\u009b31m"} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("Parse(%q) accepted a control character", bad)
		}
	}
}
