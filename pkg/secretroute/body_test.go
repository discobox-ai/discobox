package secretroute

import (
	"strings"
	"testing"
	"time"
)

// boxdExchange is the one request boxd's API key is sent in.
const boxdExchange = "https://app.boxd.sh/api/v1/auth/token" //nolint:gosec // a URL, not a credential

func TestBodyRouteNormalizeParseFormat(t *testing.T) {
	for text, want := range map[string]string{ //nolint:gosec // routes, not credentials
		"POST app.boxd.sh/api/v1/auth/token body=/api_key":         "POST app.boxd.sh/api/v1/auth/token body=/api_key",
		"POST app.boxd.sh/api/v1/auth/token body=/b,/a/0,/a/0":     "POST app.boxd.sh/api/v1/auth/token body=/a/0,/b",
		"app.boxd.sh body=/a~1b,/c~0d=blocked":                     "app.boxd.sh body=/a~1b,/c~0d=blocked",
		"POST app.boxd.sh/api/v1/auth/token body=/odd,key=blocked": "POST app.boxd.sh/api/v1/auth/token body=/odd,key=blocked",
		"POST app.boxd.sh/api/v1/auth/token body=/":                "POST app.boxd.sh/api/v1/auth/token body=/",
	} {
		route, err := Parse(text)
		if err != nil {
			t.Fatalf("Parse(%q): %v", text, err)
		}
		if got := Format(route); got != want {
			t.Fatalf("Format(Parse(%q)) = %q, want %q", text, got, want)
		}
		again, err := Parse(Format(route))
		if err != nil || Format(again) != want {
			t.Fatalf("Parse(Format) of %q = %q, %v", text, Format(again), err)
		}
	}
	for text, wantErr := range map[string]string{
		"app.boxd.sh body=":                 "JSON pointers",
		"app.boxd.sh body=api_key":          "JSON pointers",
		"app.boxd.sh body=/a~2":             "~0 or ~1",
		"app.boxd.sh body=/a~":              "~0 or ~1",
		"app.boxd.sh body=/a body=/b":       "one body=",
		"app.boxd.sh body=/a mcp=x":         "no GraphQL or MCP",
		"app.boxd.sh graphql=query body=/a": "no GraphQL or MCP",
	} {
		if _, err := Parse(text); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("Parse(%q) = %v, want %q", text, err, wantErr)
		}
	}
	for _, place := range []string{"/a=b", "/a b", "/a\x01", "/a,/b"} {
		if _, err := Normalize(Route{Hosts: []string{"x.test"}, Body: &Body{Places: []string{place}}}); err == nil {
			t.Fatalf("Normalize accepted body place %q", place)
		}
	}
	if _, err := Normalize(Route{Hosts: []string{"x.test"}, Body: &Body{}}); err == nil {
		t.Fatal("Normalize accepted a body block with no places")
	}
}

func TestDecideBodyPlaces(t *testing.T) {
	post := get("POST", boxdExchange)
	at := func(req Request, uses ...string) Request { req.At = uses; return req }
	for _, tc := range []struct {
		name    string
		routes  []string
		req     Request
		allowed bool
		reason  string
	}{
		{
			name:    "the place a route names",
			routes:  []string{"POST app.boxd.sh/api/v1/auth/token body=/api_key"},
			req:     at(post, "/api_key"),
			allowed: true,
		},
		{
			name:   "another place",
			routes: []string{"POST app.boxd.sh/api/v1/auth/token body=/api_key"},
			req:    at(post, "/other"),
			reason: "does not name the body place /other",
		},
		{
			name:   "a body route grants no header use",
			routes: []string{"POST app.boxd.sh/api/v1/auth/token body=/api_key"},
			req:    post,
			reason: "no route to POST app.boxd.sh/api/v1/auth/token",
		},
		{
			name:   "a header route grants no body place, even for any host",
			routes: []string{"*"},
			req:    at(post, "/api_key"),
			reason: "names no body place",
		},
		{
			name:   "another method",
			routes: []string{"POST app.boxd.sh/api/v1/auth/token body=/api_key"},
			req:    at(get("PUT", boxdExchange), "/api_key"),
			reason: "does not allow PUT",
		},
		{
			name:   "a method override",
			routes: []string{"POST app.boxd.sh/api/v1/auth/token body=/api_key"},
			req: func() Request {
				r := at(post, "/api_key")
				r.URL += "?_method=DELETE"
				return r
			}(),
			reason: "does not allow DELETE",
		},
		{
			name:   "another path",
			routes: []string{"POST app.boxd.sh/api/v1/auth/token body=/api_key"},
			req:    at(get("POST", "https://app.boxd.sh/api/v1/other"), "/api_key"),
			reason: "no route to the body place /api_key of POST app.boxd.sh/api/v1/other",
		},
		{
			name:    "header and body uses each find their own route",
			routes:  []string{"app.boxd.sh", "POST app.boxd.sh/api/v1/auth/token body=/api_key"},
			req:     at(post, InHeaders, "/api_key"),
			allowed: true,
		},
		{
			name:   "every use must be allowed",
			routes: []string{"POST app.boxd.sh/api/v1/auth/token body=/api_key"},
			req:    at(post, "/api_key", InHeaders),
			reason: "no route to POST app.boxd.sh/api/v1/auth/token",
		},
		{
			name:   "a blocked route without a body block refuses a body place",
			routes: []string{"app.boxd.sh/api/v1/auth/token=blocked", "POST app.boxd.sh/api/v1/auth/** body=/api_key"},
			req:    at(post, "/api_key"),
			reason: "refuses the body place /api_key of POST app.boxd.sh/api/v1/auth/token",
		},
		{
			name:   "a blocked body route refuses its place",
			routes: []string{"app.boxd.sh/api/v1/auth/token body=/api_key=blocked", "POST app.boxd.sh/api/** body=/api_key"},
			req:    at(post, "/api_key"),
			reason: "refuses",
		},
		{
			name:    "a blocked body route leaves the headers alone",
			routes:  []string{"app.boxd.sh/api/v1/auth/token body=/api_key=blocked", "app.boxd.sh"},
			req:     post,
			allowed: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allowed, reason := Decide([][]Route{routes(t, tc.routes...)}, tc.req)
			if allowed != tc.allowed || !strings.Contains(reason, tc.reason) {
				t.Fatalf("Decide = %v %q, want %v %q", allowed, reason, tc.allowed, tc.reason)
			}
		})
	}
}

func TestDecideGrantsHoldsEveryUseToOneGrant(t *testing.T) {
	headers := Grant{Lists: [][]Route{routes(t, "app.boxd.sh")}}
	body := Grant{Lists: [][]Route{routes(t, "POST app.boxd.sh/api/v1/auth/token body=/api_key")}}
	req := get("POST", boxdExchange)
	req.At = []string{InHeaders, "/api_key"}
	if allowed, _ := DecideGrants([]Grant{headers, body}, time.Now(), req); allowed {
		t.Fatal("two grants, each allowing one use, allowed both")
	}
	both := Grant{Lists: [][]Route{routes(t, "app.boxd.sh", "POST app.boxd.sh/api/v1/auth/token body=/api_key")}}
	if allowed, reason := DecideGrants([]Grant{headers, both}, time.Now(), req); !allowed {
		t.Fatalf("a grant allowing both uses refused: %s", reason)
	}
}

func TestBodyPlaces(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	grants := []Grant{
		{Lists: [][]Route{routes(t, "POST app.boxd.sh/api/v1/auth/token body=/api_key", "app.boxd.sh", "app.boxd.sh/api/v1/auth/token body=/x=blocked")}},
		{Lists: [][]Route{routes(t, "app.boxd.sh/api/** body=/b,/api_key")}},
		{Lists: [][]Route{routes(t, "app.boxd.sh body=/expired")}, ExpiresAt: &past},
	}
	for _, tc := range []struct {
		req  Request
		want string
	}{
		{get("POST", boxdExchange), "/api_key /b"},
		{get("GET", boxdExchange), "/api_key /b"},
		{get("GET", "https://app.boxd.sh/other"), ""},
		{get("POST", "https://other.test/api/v1/auth/token"), ""},
	} {
		if got := strings.Join(BodyPlaces(grants, time.Now(), tc.req), " "); got != tc.want {
			t.Fatalf("BodyPlaces(%s %s) = %q, want %q", tc.req.Method, tc.req.URL, got, tc.want)
		}
	}
}

func TestProveContainedBodyPlaces(t *testing.T) {
	secret := routes(t, "app.boxd.sh body=/api_key,/token", "app.boxd.sh")
	for _, tc := range []struct {
		grant, limit []string
		ok           bool
	}{
		{[]string{"POST app.boxd.sh/api/v1/auth/token body=/api_key"}, []string{"app.boxd.sh body=/api_key,/token"}, true},
		{[]string{"app.boxd.sh body=/api_key,/token"}, []string{"app.boxd.sh body=/api_key"}, false},
		{[]string{"app.boxd.sh body=/api_key"}, []string{"app.boxd.sh"}, false},
		{[]string{"app.boxd.sh"}, []string{"app.boxd.sh body=/api_key"}, false},
		{[]string{"app.boxd.sh/x"}, []string{"app.boxd.sh"}, true},
	} {
		err := ProveContained(secret, routes(t, tc.grant...), routes(t, tc.limit...))
		if (err == nil) != tc.ok {
			t.Fatalf("ProveContained(%q within %q) = %v, want ok %v", tc.grant, tc.limit, err, tc.ok)
		}
	}
}

func TestHostAllowedIsNotShutByABlockedBodyRoute(t *testing.T) {
	if !HostAllowed([][]Route{routes(t, "app.boxd.sh body=/api_key=blocked", "app.boxd.sh/x")}, "app.boxd.sh") {
		t.Fatal("a blocked body route shut its host")
	}
}
