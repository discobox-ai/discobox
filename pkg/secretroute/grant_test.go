package secretroute

import (
	"strings"
	"testing"
	"time"
)

// A request passes when some live grant passes it; each grant is decided
// whole, so one grant's blocked route never refuses what another allows, and
// a lapsed grant allows nothing by the caller's clock (ADR 26-11-03-329 §3).
func TestDecideGrants(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	later, earlier := now.Add(time.Hour), now.Add(-time.Second)
	secret := routes(t, "api.github.com/**")
	base := Grant{Lists: [][]Route{secret, routes(t,
		"GET api.github.com/repos/acme/web/**",
		"api.github.com/repos/acme/web/pulls=blocked",
	)}}
	added := Grant{Lists: [][]Route{secret, routes(t, "api.github.com/repos/acme/web/pulls")}, ExpiresAt: &later}
	lapsed := Grant{Lists: added.Lists, ExpiresAt: &earlier}

	for _, tc := range []struct {
		name   string
		grants []Grant
		req    Request
		allow  bool
		reason string
	}{
		{"base alone", []Grant{base}, get("GET", "https://api.github.com/repos/acme/web/issues"), true, ""},
		{"the base's block refuses within the base", []Grant{base}, get("GET", "https://api.github.com/repos/acme/web/pulls"), false, "refuses"},
		{"an addition allows what the base blocks", []Grant{base, added}, get("POST", "https://api.github.com/repos/acme/web/pulls"), true, ""},
		{"a lapsed addition allows nothing", []Grant{base, lapsed}, get("POST", "https://api.github.com/repos/acme/web/pulls"), false, "refuses"},
		{"the base still allows beside a lapsed addition", []Grant{base, lapsed}, get("GET", "https://api.github.com/repos/acme/web/issues"), true, ""},
		{"the refusal is the first live grant's", []Grant{lapsed, base}, get("DELETE", "https://api.github.com/repos/acme/web"), false, "route GET api.github.com/repos/acme/web/** does not allow DELETE"},
		{"every grant lapsed", []Grant{lapsed}, get("GET", "https://api.github.com/"), false, "expired"},
		{"no grants", nil, get("GET", "https://api.github.com/"), false, "no routes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allow, reason := DecideGrants(tc.grants, now, tc.req)
			if allow != tc.allow || !strings.Contains(reason, tc.reason) {
				t.Fatalf("DecideGrants = %v %q, want %v containing %q", allow, reason, tc.allow, tc.reason)
			}
		})
	}
}

// The body is read once across every grant, as Request.Body promises.
func TestDecideGrantsReadsTheBodyOnce(t *testing.T) {
	now := time.Now()
	gql := routes(t, "api.github.com/graphql graphql=query")
	grants := []Grant{{Lists: [][]Route{gql}}, {Lists: [][]Route{gql}}}
	calls := 0
	req := get("POST", "https://api.github.com/graphql")
	req.Body = func() ([]byte, bool) {
		calls++
		return []byte(`{"query":"mutation { x }"}`), true
	}
	if allow, _ := DecideGrants(grants, now, req); allow {
		t.Fatal("a mutation passed a query-only grant")
	}
	if calls != 1 {
		t.Fatalf("body read %d times, want 1", calls)
	}
}

func TestGrantsAllowingHost(t *testing.T) {
	now := time.Now()
	earlier, later := now.Add(-time.Minute), now.Add(time.Hour)
	github := Grant{Lists: [][]Route{routes(t, "api.github.com/**")}}
	uploads := Grant{Lists: [][]Route{routes(t, "*.github.com")}, ExpiresAt: &later}
	npm := Grant{Lists: [][]Route{routes(t, "registry.npmjs.org/**")}, ExpiresAt: &earlier}
	grants := []Grant{npm, github, uploads}
	if got := GrantsAllowingHost(grants, now, "api.github.com"); len(got) != 2 || LatestExpiry(got) != nil {
		t.Fatalf("api.github.com: %+v, want both github grants, held for good", got)
	}
	if got := GrantsAllowingHost(grants, now, "uploads.github.com"); len(got) != 1 || !LatestExpiry(got).Equal(later) {
		t.Fatalf("uploads.github.com: %+v, want the addition alone, held until it lapses", got)
	}
	if got := GrantsAllowingHost(grants, now, "registry.npmjs.org"); len(got) != 0 {
		t.Fatalf("a lapsed grant's host = %+v, want none", got)
	}
}
