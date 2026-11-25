package access

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
)

func TestAuditShowsWhyARequestWasRefused(t *testing.T) {
	svc := &fakeService{exchanges: []agentcreds.AuditExchange{{
		ID:            "http_7",
		CreatedAt:     time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC),
		Method:        "POST",
		URL:           "https://api.github.com/repos/o/r/pulls",
		Host:          "api.github.com",
		Status:        403,
		Blocked:       true,
		BlockedReason: "route: this credential has no route to api.github.com/repos/o/r/pulls",
	}}}
	serve(t, svc)
	stdout, _, code := capture(t, "", func() int {
		return Run([]string{"audit", "--blocked", "--host", "api.github.com", "--since", "10m", "--limit", "5"})
	})
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"403 BLOCKED  POST https://api.github.com/repos/o/r/pulls", "reason: route: this credential has no route to api.github.com/repos/o/r/pulls"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	got := svc.gotAudit
	if got.Host != "api.github.com" || got.Blocked == nil || !*got.Blocked || got.Limit != 5 || time.Since(got.Since) < 9*time.Minute || time.Since(got.Since) > 11*time.Minute {
		t.Fatalf("service saw %+v", got)
	}
}

// A 401 answered to a request sent with a delivered credential says so
// (#1745): the credential was what was refused, not missing.
func TestAuditSaysA401CarriedACredential(t *testing.T) {
	serve(t, &fakeService{exchanges: []agentcreds.AuditExchange{
		{ID: "http_9", CreatedAt: time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC), Method: "GET", URL: "https://api.github.com/repos", Host: "api.github.com", Status: 200, CredentialCarried: true},
		{ID: "http_8", CreatedAt: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC), Method: "GET", URL: "https://api.github.com/user", Host: "api.github.com", Status: 401, CredentialCarried: true},
		{ID: "http_7", CreatedAt: time.Date(2026, 11, 24, 11, 0, 0, 0, time.UTC), Method: "GET", URL: "https://example.com/", Host: "example.com", Status: 401},
	}})
	stdout, _, code := capture(t, "", func() int { return Run([]string{"audit"}) })
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if got := strings.Count(stdout, "sent with a delivered credential"); got != 1 || !strings.Contains(stdout, "401  GET https://api.github.com/user\n    sent with a delivered credential") {
		t.Fatalf("stdout:\n%s\nwant the note under the 401 that carried a credential, and only there", stdout)
	}
}

func TestAuditJSONIsTheProtocolBody(t *testing.T) {
	serve(t, &fakeService{})
	stdout, _, code := capture(t, "", func() int { return Run([]string{"audit", "--json", "--allowed"}) })
	var body agentcreds.AuditHTTPResponse
	if code != exitOK || json.Unmarshal([]byte(stdout), &body) != nil || body.Exchanges == nil || len(body.Exchanges) != 0 {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
}

func TestAuditRefusesContradictoryFilters(t *testing.T) {
	serve(t, &fakeService{})
	for _, args := range [][]string{
		{"audit", "--json", "--blocked", "--allowed"},
		{"audit", "--json", "--limit", "0"},
		{"audit", "--json", "stray"},
	} {
		_, stderr, code := capture(t, "", func() int { return Run(args) })
		if code != exitUsage || !strings.Contains(stderr, `"code":"invalid"`) {
			t.Errorf("%v: exit %d, stderr %s", args, code, stderr)
		}
	}
}
