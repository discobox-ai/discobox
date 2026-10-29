package secretroute

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/hostscope"
)

// routes parses typed routes, failing the test on a bad one.
func routes(t *testing.T, texts ...string) []Route {
	t.Helper()
	out := make([]Route, 0, len(texts))
	for _, text := range texts {
		route, err := Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, route)
	}
	return out
}

// get is a request with an empty body.
func get(method, rawURL string) Request {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	return Request{Method: method, Host: u.Hostname(), URL: rawURL, Body: func() ([]byte, bool) { return nil, true }}
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

func TestHostPatterns(t *testing.T) {
	for _, tc := range []struct {
		pattern, host string
		want          bool
	}{
		{"api.github.com", "api.github.com", true},
		{"API.github.com", "api.github.com", true},
		// A host no longer covers the hosts beneath it: that is *.h.
		{"github.com", "api.github.com", false},
		{"*.github.com", "api.github.com", true},
		{"*.github.com", "a.b.github.com", true},
		{"*.github.com", "github.com", false},
		{"*.github.com", "github.com.evil.test", false},
		{"*.github.com", "evilgithub.com", false},
		{"*", "anything.test", true},
		{"*", "", false},
	} {
		_, got := hostPatternScore(tc.pattern, tc.host)
		if got != tc.want {
			t.Fatalf("host pattern %q on %q = %v, want %v", tc.pattern, tc.host, got, tc.want)
		}
	}
}

func TestPathPatterns(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{"/", "/", true},
		{"/", "/a", false},
		{"/a", "/a", true},
		{"/a", "/A", true},
		{"/a", "/a/b", false}, // a literal path is the whole path
		{"/a", "/ab", false},
		{"/a/*", "/a/b", true},
		{"/a/*", "/a", false},
		{"/a/*", "/a/b/c", false},
		{"/a/*/c", "/a/b/c", true},
		{"/a/**", "/a", true},
		{"/a/**", "/a/b/c", true},
		{"/a/**", "/ab", false},
		{"/**", "/", true},
		{"/**", "/x/y", true},
		{"/*", "/", false},
	} {
		if got := pathMatches(splitPath(tc.pattern), splitPath(tc.path)); got != tc.want {
			t.Fatalf("path pattern %q on %q = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

// Within a list the most specific candidates decide: host first, then path;
// methods and protocol blocks pick candidates and never rank them.
func TestDecideSpecificity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		routes []string
		cases  map[string]bool // "METHOD URL" -> allowed
	}{
		{
			name:   "a blocked carve-out under a whole host",
			routes: []string{"api.github.com", "api.github.com/repos/*/*/pulls/*/merge=blocked"},
			cases: map[string]bool{
				"PUT https://api.github.com/repos/a/b/pulls/1/merge":        false,
				"GET https://api.github.com/repos/a/b/pulls/1":              true,
				"PUT https://api.github.com/repos/a/b/pulls/1/merge/x":      true, // the pattern is the whole path
				"PUT https://api.github.com/repos/a/b/pulls/1/x/../merge":   false,
				"PUT https://api.github.com/repos/a/b/pulls/1%2Fx/../merge": false,
				"GET https://uploads.github.com/x":                          false,
			},
		},
		{
			name:   "methods pick candidates",
			routes: []string{"GET api.github.com", "POST api.github.com/repos/*/*/pulls"},
			cases: map[string]bool{
				"GET https://api.github.com/anything":          true,
				"get https://api.github.com/anything":          true,
				"POST https://api.github.com/repos/a/b/pulls":  true,
				"POST https://api.github.com/repos/a/b/issues": false,
				"DELETE https://api.github.com/repos/a/b":      false,
			},
		},
		{
			name:   "a blocked method on a path refuses only that method",
			routes: []string{"api.github.com", "DELETE api.github.com/repos/**=blocked"},
			cases: map[string]bool{
				"GET https://api.github.com/repos/a/b":    true,
				"DELETE https://api.github.com/repos/a/b": false,
				"DELETE https://api.github.com/user":      true,
			},
		},
		{
			name:   "more literal segments beat a wildcard",
			routes: []string{"x.test/a/*=blocked", "x.test/a/b"},
			cases: map[string]bool{
				"GET https://x.test/a/b": true,
				"GET https://x.test/a/c": false,
			},
		},
		{
			name:   "then more segments, then without **",
			routes: []string{"x.test/a/**", "x.test/a/*=blocked", "x.test/a/*/c"},
			cases: map[string]bool{
				"GET https://x.test/a":       true,  // only /a/** matches
				"GET https://x.test/a/b":     false, // /a/* (2 segments) beats /a/** (1 segment, with **)
				"GET https://x.test/a/b/c":   true,  // /a/*/c has 3 segments
				"GET https://x.test/a/b/c/d": true,
			},
		},
		{
			// A trailing ** matches zero segments, so it never outranks the
			// same path without it.
			name:   "an exact blocked path carves itself out of its own /**",
			routes: []string{"api.github.com/repos/acme/web/**", "DELETE api.github.com/repos/acme/web=blocked"},
			cases: map[string]bool{
				"DELETE https://api.github.com/repos/acme/web":       false,
				"GET https://api.github.com/repos/acme/web":          true,
				"DELETE https://api.github.com/repos/acme/web/pulls": true,
			},
		},
		{
			name:   "an exact path outranks its own /**",
			routes: []string{"x.test/a/**", "x.test/a=blocked"},
			cases: map[string]bool{
				"GET https://x.test/a":   false,
				"GET https://x.test/a/b": true,
			},
		},
		{
			name:   "a blocked route ties and wins",
			routes: []string{"x.test/a", "x.test/a=blocked"},
			cases:  map[string]bool{"GET https://x.test/a": false},
		},
		{
			name:   "host before path",
			routes: []string{"*.github.com/a/b/c=blocked", "api.github.com"},
			cases: map[string]bool{
				"GET https://api.github.com/a/b/c":     true,
				"GET https://uploads.github.com/a/b/c": false,
			},
		},
		{
			name:   "an exact host beats a wildcard, and a longer suffix a shorter",
			routes: []string{"*.github.com", "*.api.github.com=blocked", "x.api.github.com"},
			cases: map[string]bool{
				"GET https://y.api.github.com/": false,
				"GET https://x.api.github.com/": true,
				"GET https://api.github.com/":   true,
				"GET https://github.com/":       false,
			},
		},
		{
			name:   "* is least",
			routes: []string{"*", "internal.example=blocked", "*.internal.example=blocked"},
			cases: map[string]bool{
				"GET https://registry.npmjs.org/x":      true,
				"GET https://internal.example/":         false,
				"GET https://ci.internal.example/":      false,
				"GET https://internal.example.evil.com": true,
			},
		},
		{
			name:   "a route with no paths is less specific than /**",
			routes: []string{"x.test=blocked", "x.test/**"},
			cases:  map[string]bool{"GET https://x.test/a": true},
		},
		{
			name:   "an encoded slash is held to both readings",
			routes: []string{"x.test", "x.test/mcp/**=blocked", "x.test/mcp/readonly/**"},
			cases: map[string]bool{
				"GET https://x.test/mcp/readonly/x":               true,
				"GET https://x.test/mcp/readonly/../write":        false,
				"GET https://x.test/mcp/readonly%2F..%2F..%2Fmcp": false,
				"GET https://x.test/mcp/x%2F..%2Freadonly":        false,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list := routes(t, tc.routes...)
			for request, want := range tc.cases {
				method, rawURL, _ := strings.Cut(request, " ")
				allowed, reason := Decide([][]Route{list}, get(method, rawURL))
				if allowed != want {
					t.Fatalf("Decide(%s) = %v (%s), want %v", request, allowed, reason, want)
				}
				if !allowed && reason == "" {
					t.Fatalf("Decide(%s) refused without a reason", request)
				}
			}
		})
	}
}

// A refusal says what refused: the blocked route and what it refuses, or,
// when no route is a candidate, the most specific route that failed only on
// its methods or protocol block, and why.
func TestDecideReasons(t *testing.T) {
	jsonType := http.Header{"Content-Type": {"application/json"}}
	withBody := func(req Request, header http.Header, body string, readable bool) Request {
		req.Header = header
		req.Body = func() ([]byte, bool) { return []byte(body), readable }
		return req
	}
	const graphql = "https://172.17.0.1/graphql"
	const mcp = "https://api.githubcopilot.com/mcp"
	for _, tc := range []struct {
		routes []string
		req    Request
		want   string
	}{
		{
			[]string{"x.test", "x.test/admin/**=blocked"},
			get("POST", "https://x.test/admin/x"),
			"route x.test/admin/**=blocked refuses POST x.test/admin/x",
		},
		{
			[]string{"x.test"},
			get("GET", "https://y.test/"),
			"this credential has no route to GET y.test/",
		},
		{
			[]string{"x.test"},
			get("GET", "https://y.test/a%1b[2Jb"),
			`this credential has no route to GET "y.test/a\x1b[2Jb"`,
		},
		{
			[]string{"172.17.0.1/graphql graphql=query"},
			post(graphql, `{"query":"mutation { deleteRepository(input: {}) { x } }"}`),
			"route 172.17.0.1/graphql graphql=query does not allow a mutation",
		},
		{
			[]string{"172.17.0.1/graphql graphql=mutation:createPullRequest"},
			post(graphql, `{"query":"mutation { deleteRepository(input: {}) { x } }"}`),
			"route 172.17.0.1/graphql graphql=mutation:createPullRequest does not allow the root field deleteRepository",
		},
		{
			[]string{"172.17.0.1/graphql graphql=query"},
			withBody(get("POST", graphql), jsonType, "", false),
			"route 172.17.0.1/graphql graphql=query needs the body, which could not be read (too large, of undeclared length, or in an encoding the proxy cannot decode)",
		},
		{
			[]string{"172.17.0.1/graphql graphql=query"},
			post(graphql, `{"extensions":{}}`),
			"route 172.17.0.1/graphql graphql=query could not parse the GraphQL request (no document, a persisted query, or one that does not parse)",
		},
		{
			[]string{"172.17.0.1/graphql", "172.17.0.1/graphql graphql=mutation:delete*=blocked"},
			post(graphql, `{"query":"mutation { deleteRepository(input: {}) { x } }"}`),
			"route 172.17.0.1/graphql graphql=mutation:delete*=blocked refuses a mutation of the root field deleteRepository",
		},
		{
			[]string{"api.githubcopilot.com/mcp mcp=get_*"},
			post(mcp, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_file"}}`),
			"route api.githubcopilot.com/mcp mcp=get_* does not allow the tool write_file",
		},
		{
			// A tool name from the body is escaped.
			[]string{"api.githubcopilot.com/mcp mcp=get_*"},
			post(mcp, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x\u001b]0;y"}}`),
			`route api.githubcopilot.com/mcp mcp=get_* does not allow the tool "x\x1b]0;y"`,
		},
		{
			[]string{"api.githubcopilot.com/mcp mcp=*"},
			post(mcp, `{"jsonrpc":"2.0","id":1,"method":"resources/read"}`),
			"route api.githubcopilot.com/mcp mcp=* does not allow the MCP method resources/read",
		},
		{
			[]string{"api.githubcopilot.com/mcp", "api.githubcopilot.com/mcp mcp=delete_*=blocked"},
			post(mcp, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_file"}}`),
			"route api.githubcopilot.com/mcp mcp=delete_*=blocked refuses the tool delete_file",
		},
		{
			[]string{"api.x.test/**", "DELETE api.x.test/**=blocked"},
			withBody(get("POST", "https://api.x.test/a"), http.Header{"X-Http-Method-Override": {"DELETE"}}, "", true),
			"route DELETE api.x.test/**=blocked refuses DELETE, named by X-HTTP-Method-Override",
		},
		{
			[]string{"api.x.test/**", "DELETE api.x.test/**=blocked"},
			withBody(get("POST", "https://api.x.test/a"), jsonType, `{"_method":"DELETE"}`, true),
			"route DELETE api.x.test/**=blocked refuses DELETE, named by _method in the body",
		},
		{
			[]string{"api.x.test/**", "DELETE api.x.test/**=blocked"},
			withBody(get("POST", "https://api.x.test/a"), jsonType, ``, false),
			"route DELETE api.x.test/**=blocked refuses a POST whose body could not be read for its _method",
		},
		{
			[]string{"POST api.x.test/things/*"},
			withBody(get("POST", "https://api.x.test/things/1?_method=DELETE"), nil, "", true),
			"route POST api.x.test/things/* does not allow DELETE, named by _method in the URL",
		},
		{
			[]string{"POST api.x.test/things/*"},
			withBody(get("POST", "https://api.x.test/things/1"), jsonType, `{"_method":"DELETE","_method":"DELETE"}`, true),
			"route POST api.x.test/things/* does not allow DELETE, named by _method in the body",
		},
		{
			[]string{"POST api.x.test/things/*"},
			withBody(get("POST", "https://api.x.test/things/1"), nil, "", false),
			"route POST api.x.test/things/* needs the body, which could not be read (too large, of undeclared length, or in an encoding the proxy cannot decode)",
		},
		{
			[]string{"POST api.x.test/things/*"},
			get("PUT", "https://api.x.test/things/1"),
			"route POST api.x.test/things/* does not allow PUT",
		},
		{
			// The most specific near miss is named.
			[]string{"GET api.x.test", "POST api.x.test/things/*"},
			get("PUT", "https://api.x.test/things/1"),
			"route POST api.x.test/things/* does not allow PUT",
		},
	} {
		_, reason := Decide([][]Route{routes(t, tc.routes...)}, tc.req)
		if reason != tc.want {
			t.Errorf("Decide(%v) reason\n got %q\nwant %q", tc.routes, reason, tc.want)
		}
	}
}

// A credential is sent where its routes say and only there: no routes, or no
// lists at all, allows nothing (ADR 26-10-27-880 §1).
func TestDecideAllowsNothingWithoutRoutes(t *testing.T) {
	for _, lists := range [][][]Route{nil, {}, {nil}, {{{Hosts: []string{"example.com"}}}, {}}} {
		if allowed, reason := Decide(lists, get("GET", "https://example.com/")); allowed || reason == "" {
			t.Fatalf("Decide(%v) = %v %q, want a refusal", lists, allowed, reason)
		}
	}
}

// Every list must allow: a binding narrows its secret, and a parent narrows
// the binding handed on under it. No list unblocks what another blocks.
func TestDecideHoldsARequestToEveryList(t *testing.T) {
	secret := routes(t, "github.com,*.github.com", "github.com/admin/**=blocked")
	binding := routes(t, "api.github.com/repos/acme/**")
	parent := routes(t, "*")
	lists := [][]Route{secret, binding, parent}
	for _, tc := range []struct {
		url     string
		allowed bool
	}{
		{"https://api.github.com/repos/acme/web/pulls", true},
		{"https://api.github.com/repos/other/web", false}, // off the binding
		{"https://github.com/admin", false},               // off the binding, and blocked by the secret
	} {
		if allowed, reason := Decide(lists, get("GET", tc.url)); allowed != tc.allowed {
			t.Fatalf("Decide(%s) = %v (%s), want %v", tc.url, allowed, reason, tc.allowed)
		}
	}
	// A binding's open route cannot reopen a path its secret blocks.
	reopen := [][]Route{secret, routes(t, "github.com/admin/**")}
	if allowed, _ := Decide(reopen, get("GET", "https://github.com/admin/x")); allowed {
		t.Fatal("a binding reopened a path its secret blocks")
	}
	// A parent's method limit holds the binding under it.
	methods := [][]Route{routes(t, "api.github.com"), routes(t, "GET api.github.com")}
	if allowed, _ := Decide(methods, get("POST", "https://api.github.com/x")); allowed {
		t.Fatal("a list without the method allowed it")
	}
}

func TestHostAllowed(t *testing.T) {
	lists := [][]Route{
		routes(t, "github.com,*.github.com", "github.com/admin/**=blocked"),
		routes(t, "api.github.com"),
	}
	if !HostAllowed(lists, "API.github.com:443") {
		t.Fatal("a host every list opens was refused")
	}
	if HostAllowed(lists, "uploads.github.com") {
		t.Fatal("a host one list does not open was allowed")
	}
	if HostAllowed(nil, "api.github.com") || HostAllowed([][]Route{{}}, "api.github.com") {
		t.Fatal("no routes allowed a host")
	}
	for _, tc := range []struct {
		name   string
		routes []string
		host   string
		want   bool
	}{
		{"only a block", []string{"api.github.com=blocked"}, "api.github.com", false},
		{"a host-wide block shuts a wider open route", []string{"*.github.com", "uploads.github.com=blocked"}, "uploads.github.com", false},
		{"a sibling the block does not cover", []string{"*.github.com", "uploads.github.com=blocked"}, "api.github.com", true},
		{"a blocked path leaves the host open", []string{"*.github.com", "uploads.github.com/x=blocked"}, "uploads.github.com", true},
		{"a blocked method is not host-wide", []string{"*.github.com", "POST uploads.github.com=blocked"}, "uploads.github.com", true},
		{"a blocked protocol is not host-wide", []string{"*.github.com", "uploads.github.com mcp=*=blocked"}, "uploads.github.com", true},
		{"an open path on a blocked host", []string{"github.com=blocked", "github.com/acme/**"}, "github.com", true},
		{"an open method on a blocked host ties and loses", []string{"github.com=blocked", "GET github.com"}, "github.com", false},
		{"a named route under a blocked *", []string{"*=blocked", "api.github.com"}, "api.github.com", true},
		{"an unnamed host under a blocked *", []string{"*=blocked", "api.github.com"}, "example.com", false},
		{"an exact host does not cover beneath it", []string{"github.com"}, "api.github.com", false},
	} {
		if got := HostAllowed([][]Route{routes(t, tc.routes...)}, tc.host); got != tc.want {
			t.Fatalf("%s: HostAllowed(%v, %s) = %v, want %v", tc.name, tc.routes, tc.host, got, tc.want)
		}
	}
	// An open path HostAllowed admits on a blocked host, Decide allows.
	path := [][]Route{routes(t, "github.com=blocked", "github.com/acme/**")}
	if allowed, _ := Decide(path, get("GET", "https://github.com/acme/web")); !allowed {
		t.Fatal("Decide refused the open path HostAllowed admitted")
	}
}

// legacyRoute and legacyDecide are the routes and the decision this package
// made before routes named host patterns: a host covered itself and the hosts
// beneath it, a path was a prefix on whole segments, and the most host
// labels, then the longest path, decided.
type legacyRoute struct {
	host, path string
	blocked    bool
}

func legacyDecide(routes []legacyRoute, host, rawURL string) bool {
	for _, requestPath := range RequestPaths(rawURL) {
		bestLabels, bestPath, blocked, found := -1, -1, false, false
		for _, route := range routes {
			covers := route.host == AnyHost || hostscope.Covers(route.host, host)
			prefix := route.path == "" || (len(requestPath) >= len(route.path) && strings.EqualFold(requestPath[:len(route.path)], route.path) &&
				(len(requestPath) == len(route.path) || requestPath[len(route.path)] == '/'))
			if !covers || !prefix {
				continue
			}
			labels := 0
			if route.host != AnyHost {
				labels = strings.Count(route.host, ".") + 1
			}
			switch {
			case labels > bestLabels || (labels == bestLabels && len(route.path) > bestPath):
				bestLabels, bestPath, blocked, found = labels, len(route.path), route.blocked, true
			case labels == bestLabels && len(route.path) == bestPath:
				blocked = blocked || route.blocked
			}
		}
		if !found || blocked {
			return false
		}
	}
	return true
}

// A legacy route list and its conversion decide every request alike: the
// migration neither narrows nor widens (ADR 26-10-27-880 §8).
func TestFromLegacyDecidesAsBefore(t *testing.T) {
	hosts := []string{"*", "github.com", "api.github.com", "x.api.github.com", "example.com", "com"}
	paths := []string{"", "/a", "/a/b", "/A/b", "/c"}
	requestHosts := []string{"github.com", "api.github.com", "x.api.github.com", "y.github.com", "example.com", "a.example.com", "evilgithub.com", "registry.npmjs.org"}
	requestPaths := []string{"/", "/a", "/a/b", "/a/b/c", "/ab", "/a/c", "/c/d", "/A/B", "/a/x%2F..%2Fb"}
	random := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // a seeded, reproducible sample of route lists, not a secret
	for i := range 2000 {
		var legacy []legacyRoute
		var converted []Route
		for range 1 + random.IntN(4) {
			route := legacyRoute{host: hosts[random.IntN(len(hosts))], path: paths[random.IntN(len(paths))], blocked: random.IntN(3) == 0}
			legacy = append(legacy, route)
			newRoute, err := FromLegacy(route.host, route.path, route.blocked)
			if err != nil {
				t.Fatal(err)
			}
			converted = append(converted, newRoute)
		}
		for _, host := range requestHosts {
			legacyHostOpen := false
			for _, requestPath := range requestPaths {
				rawURL := "https://" + host + requestPath
				want := legacyDecide(legacy, host, rawURL)
				legacyHostOpen = legacyHostOpen || want
				got, reason := Decide([][]Route{converted}, Request{Method: "GET", Host: host, URL: rawURL})
				if got != want {
					t.Fatalf("case %d: %v converted to %v decides %s %v (%s), legacy %v", i, legacy, formatAll(converted), rawURL, got, reason, want)
				}
			}
			// HostAllowed admits a host where some path is open, and is
			// never narrower than Decide on the paths tried.
			if legacyHostOpen && !HostAllowed([][]Route{converted}, host) {
				t.Fatalf("case %d: %v refuses %s though a path on it is open", i, formatAll(converted), host)
			}
		}
	}
}

func formatAll(list []Route) []string {
	out := make([]string, 0, len(list))
	for _, route := range list {
		out = append(out, Format(route))
	}
	return out
}

// The body is read only when a candidate route has a protocol block, and at
// most once however many routes ask.
func TestDecideReadsTheBodyLazilyAndOnce(t *testing.T) {
	calls := 0
	body := func() ([]byte, bool) {
		calls++
		return []byte(`{"query":"{ viewer { login } }"}`), true
	}
	plain := routes(t, "api.github.com", "api.github.com/repos/**=blocked")
	if allowed, _ := Decide([][]Route{plain}, Request{Method: "POST", Host: "api.github.com", URL: "https://api.github.com/graphql", Body: body}); !allowed || calls != 0 {
		t.Fatalf("allowed %v, body read %d times without a protocol block", allowed, calls)
	}
	offPath := routes(t, "api.github.com", "api.github.com/other graphql=mutation=blocked")
	if allowed, _ := Decide([][]Route{offPath}, Request{Method: "POST", Host: "api.github.com", URL: "https://api.github.com/graphql", Body: body}); !allowed || calls != 0 {
		t.Fatalf("allowed %v, body read %d times for a protocol route off the path", allowed, calls)
	}
	several := routes(t, "api.github.com/graphql graphql=query", "api.github.com/graphql graphql=mutation=blocked", "api.github.com/graphql graphql=*:viewer")
	lists := [][]Route{several, several}
	if allowed, reason := Decide(lists, Request{Method: "POST", Host: "api.github.com", URL: "https://api.github.com/graphql", Body: body}); !allowed || calls != 1 {
		t.Fatalf("allowed %v (%s), body read %d times, want once", allowed, reason, calls)
	}
}

// A request is held to every method it names: its request line's, its
// override headers', and _method in its URL or URL-encoded form body.
func TestDecideMethodOverrides(t *testing.T) {
	open := routes(t, "GET,POST api.example.com/**")
	carveOut := routes(t, "api.example.com/**", "DELETE api.example.com/**=blocked")
	form := http.Header{"Content-Type": {"application/x-www-form-urlencoded; charset=utf-8"}}
	request := func(method, rawURL string, header http.Header, body string, readable bool) Request {
		req := get(method, rawURL)
		req.Header = header
		req.Body = func() ([]byte, bool) { return []byte(body), readable }
		return req
	}
	jsonType := http.Header{"Content-Type": {"application/json; charset=utf-8"}}
	const files = "https://api.example.com/files/1"
	for _, tc := range []struct {
		name           string
		req            Request
		open, carveOut bool
	}{
		{"the request line alone", request("POST", files, nil, "", true), true, true},
		{"X-HTTP-Method-Override", request("POST", files, http.Header{"X-Http-Method-Override": {"DELETE"}}, "", true), false, false},
		{"X-HTTP-Method", request("POST", files, http.Header{"X-Http-Method": {"delete"}}, "", true), false, false},
		{"X-Method-Override", request("POST", files, http.Header{"X-Method-Override": {"DELETE"}}, "", true), false, false},
		{"an override the route lists", request("POST", files, http.Header{"X-Http-Method-Override": {"GET"}}, "", true), true, true},
		{"one of several overrides", request("POST", files, http.Header{"X-Http-Method-Override": {"GET", "PUT, DELETE"}}, "", true), false, false},
		{"_method in the URL", request("POST", files+"?_method=DELETE", nil, "", true), false, false},
		{"_method in a form body", request("POST", files, form, "a=1&_method=delete", true), false, false},
		{"a form body without _method", request("POST", files, form, "a=1", true), true, true},
		{"a form body that cannot be read", request("POST", files, form, "", false), false, false},
		{"a form body that does not parse", request("POST", files, form, "a=%zz", true), false, false},
		{"_method in a body that is neither a form nor JSON", request("POST", files, http.Header{"Content-Type": {"text/plain"}}, "_method=DELETE", true), true, true},
		// A multipart body is not parsed: an open route is held to the
		// other methods, so uploads keep working, and a blocked method
		// route refuses it, since its _method is unknown.
		{"a multipart upload", request("POST", files, http.Header{"Content-Type": {"multipart/form-data; boundary=x"}}, "--x\r\nContent-Disposition: form-data; name=\"_method\"\r\n\r\nDELETE\r\n--x--", true), true, false},
		{"a multipart/mixed body naming _method", request("POST", files, http.Header{"Content-Type": {"multipart/mixed; boundary=x"}}, "--x\r\nContent-Disposition: form-data; name=\"_method\"\r\n\r\nDELETE\r\n--x--", true), true, false},
		{"a multipart/related body", request("POST", files, http.Header{"Content-Type": {"Multipart/Related; boundary=x"}}, "", true), true, false},
		{"a multipart upload with an override header", request("POST", files, http.Header{"Content-Type": {"Multipart/Form-Data; boundary=x"}, "X-Http-Method-Override": {"DELETE"}}, "", true), false, false},
		// A body _method counts only on a POST. On a POST an open route may
		// ignore a body only when it is unreadable, never when it is
		// unparsable: a JSON body Body could not return, or a multipart
		// one. A form or untyped body that cannot be read, and any body
		// read that does not parse or is ambiguous, refuses an open route.
		{"_method in a PUT's JSON body", request("PUT", files, jsonType, `{"_method":"DELETE"}`, true), false, true},
		{"_method in a PUT's form body", request("PUT", files, form, "_method=DELETE", true), false, true},
		// A POST with no Content-Type is parsed as a form (Rack, Rails,
		// Sinatra); a GET with none is not read.
		{"_method in a POST without a Content-Type", request("POST", files, nil, "_method=DELETE", true), false, false},
		{"a POST without a Content-Type or _method", request("POST", files, nil, "a=1", true), true, true},
		{"a POST without a Content-Type that cannot be read", request("POST", files, nil, "", false), false, false},
		{"a POST without a Content-Type that does not parse", request("POST", files, nil, "a=%zz", true), false, false},
		{"a GET without a Content-Type", request("GET", files, nil, "_method=DELETE", false), true, true},
		// A JSON body's top-level _method (Laravel).
		{"_method in a JSON body", request("POST", files, jsonType, `{"a":1,"_method":"DELETE"}`, true), false, false},
		{"_method in a +json body", request("POST", files, http.Header{"Content-Type": {"application/vnd.api+json"}}, `{"_method":"delete"}`, true), false, false},
		{"_method the route lists in a JSON body", request("POST", files, jsonType, `{"_method":"GET"}`, true), true, true},
		{"a JSON body without _method", request("POST", files, jsonType, `{"a":{"_method":"DELETE"}}`, true), true, true},
		{"a null _method in a JSON body", request("POST", files, jsonType, `{"_method":null}`, true), true, true},
		{"a JSON array", request("POST", files, jsonType, `[{"_method":"DELETE"}]`, true), true, true},
		{"an empty JSON body", request("POST", files, jsonType, ``, true), true, true},
		{"a duplicate _method in a JSON body", request("POST", files, jsonType, `{"_method":"GET","_method":"DELETE"}`, true), false, false},
		{"a _method in another case in a JSON body", request("POST", files, jsonType, `{"_METHOD":"DELETE"}`, true), false, false},
		{"a _method that is not a string", request("POST", files, jsonType, `{"_method":["DELETE"]}`, true), false, false},
		{"a JSON body that does not parse", request("POST", files, jsonType, `{"_method":`, true), false, false},
		{"a JSON body that cannot be read", request("POST", files, jsonType, ``, false), true, false},
		{"a query string that does not parse", request("POST", files+"?_method=%zz", nil, "", true), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if allowed, reason := Decide([][]Route{open}, tc.req); allowed != tc.open {
				t.Errorf("open: Decide = %v (%s), want %v", allowed, reason, tc.open)
			}
			if allowed, reason := Decide([][]Route{carveOut}, tc.req); allowed != tc.carveOut {
				t.Errorf("carve-out: Decide = %v (%s), want %v", allowed, reason, tc.carveOut)
			}
		})
	}
	// A large JSON POST, such as an LLM request, whose body the proxy cannot
	// read keeps working under an open method route, and a blocked method
	// route beside it refuses it, since its _method is unknown.
	large := request("POST", "https://api.x.test/v1/messages", jsonType, "", false)
	if allowed, reason := Decide([][]Route{routes(t, "POST api.x.test/v1/messages")}, large); !allowed {
		t.Fatalf("an over-limit JSON POST was refused under an open method route: %s", reason)
	}
	if allowed, _ := Decide([][]Route{routes(t, "api.x.test/**", "DELETE api.x.test/**=blocked")}, large); allowed {
		t.Fatal("an over-limit JSON POST stepped around a blocked method route")
	}
	// A body that was read is never ignored: what parsing refuses still
	// names its method, and an ambiguous one refuses an open route.
	things := routes(t, "POST api.x.test/things/*")
	for _, tc := range []struct {
		name string
		req  Request
	}{
		{"a duplicate JSON _method", request("POST", "https://api.x.test/things/1", jsonType, `{"_method":"DELETE","_method":"DELETE"}`, true)},
		{"a case-variant JSON _method", request("POST", "https://api.x.test/things/1", jsonType, `{"_method":"DELETE","_Method":"x"}`, true)},
		{"a form _method beside a pair ParseQuery refuses", request("POST", "https://api.x.test/things/1", form, "_method=DELETE&a=b;c", true)},
		{"an untyped _method beside a pair ParseQuery refuses", request("POST", "https://api.x.test/things/1", nil, "a=b;c&_method=DELETE", true)},
		{"a form body that cannot be read", request("POST", "https://api.x.test/things/1", form, "", false)},
		{"an untyped body that cannot be read", request("POST", "https://api.x.test/things/1", nil, "", false)},
	} {
		if allowed, _ := Decide([][]Route{things}, tc.req); allowed {
			t.Fatalf("%s was allowed under an open POST route", tc.name)
		}
	}
	if allowed, reason := Decide([][]Route{things}, request("POST", "https://api.x.test/things/1", jsonType, "", false)); !allowed {
		t.Fatalf("an unreadable JSON POST was refused under an open POST route: %s", reason)
	}
	for body, want := range map[string][]string{
		"_method=DELETE&a=b;c": {"DELETE"},
		"a=1;_method=put":      {"PUT"},
		"_method=%zz":          {"%ZZ"},
	} {
		if got, _ := formMethodOverride(body); !slices.Equal(got, want) {
			t.Fatalf("formMethodOverride(%q) = %q, want %q", body, got, want)
		}
	}
	if got, ok := jsonMethodOverride([]byte(`{"_method":"GET","_METHOD":"DELETE"}`)); ok || !slices.Equal(got, []string{"GET", "DELETE"}) {
		t.Fatalf("jsonMethodOverride named %q, %v; want every value seen, and unknown", got, ok)
	}
	// A PUT is judged as a PUT, whatever its body's _method says.
	put := request("PUT", "https://api.x.test/v1/files/1", jsonType, `{"_method":"DELETE"}`, true)
	if allowed, reason := Decide([][]Route{routes(t, "PUT api.x.test/v1/files/*")}, put); !allowed {
		t.Fatalf("a PUT with a body _method was refused under PUT: %s", reason)
	}
	if allowed, reason := Decide([][]Route{routes(t, "api.x.test/**", "DELETE api.x.test/**=blocked")}, put); !allowed {
		t.Fatalf("a PUT with a body _method was refused by a DELETE carve-out: %s", reason)
	}
	// Neither reads a multipart body.
	read := false
	multipart := request("POST", files, http.Header{"Content-Type": {"multipart/form-data; boundary=x"}}, "", true)
	multipart.Body = func() ([]byte, bool) { read = true; return nil, false }
	for _, list := range [][]Route{open, carveOut} {
		Decide([][]Route{list}, multipart)
	}
	if read {
		t.Fatal("a multipart body was read for its _method")
	}
	// A route without methods never reads a form body.
	read = false
	req := request("POST", files, form, "_method=DELETE", true)
	req.Body = func() ([]byte, bool) { read = true; return nil, false }
	if allowed, _ := Decide([][]Route{routes(t, "api.example.com")}, req); !allowed || read {
		t.Fatalf("allowed %v, body read %v, for a route without methods", allowed, read)
	}
}

func ExampleFormat() {
	route, _ := Parse("POST api.github.com/repos/*/*/pulls")
	fmt.Println(Format(route))
	// Output: POST api.github.com/repos/*/*/pulls
}

// A name a request makes up is cut short in a reason, so a box cannot make its
// refusal, and the audit row that keeps it, as long as its body.
func TestReasonsCutLongNamesShort(t *testing.T) {
	long := strings.Repeat("a", 4096)
	got := safe(long)
	if n := len([]rune(got)); n != reasonNameLimit+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("safe(4096 runes) = %d runes ending %q, want %d and an ellipsis", n, got[len(got)-8:], reasonNameLimit+1)
	}
	if got := safe("write_file"); got != "write_file" {
		t.Fatalf("safe(short) = %q, want it unchanged", got)
	}
}
