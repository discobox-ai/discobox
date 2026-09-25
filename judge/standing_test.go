package judge_test

import (
	"testing"
	"time"

	"github.com/discobox-ai/discobox/judge"
)

// An allow may carry the route it stands for, and only an allow may: beside a
// refusal or an ask, a standing route is permission nobody gave.
func TestDecodeReadsAStandingAllowAndNothingLikeOne(t *testing.T) {
	t.Parallel()
	answer, err := judge.Decode([]byte(`{"allow":true,"reason":"paging its own PR's comments","standing":{"route":"GET /repos/org/repo/pulls/{n}/comments","seconds":600}}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if !answer.Allow || answer.Standing == nil || answer.Standing.Route != "GET /repos/org/repo/pulls/{n}/comments" || answer.Standing.Seconds != 600 {
		t.Fatalf("answer = %+v, standing = %+v", answer, answer.Standing)
	}
	for _, tc := range []struct{ name, output string }{
		{"beside a refusal", `{"allow":false,"reason":"no","standing":{"route":"GET /a","seconds":60}}`},
		{"beside an ask", `{"need":{"body":"json"},"reason":"body","standing":{"route":"GET /a","seconds":60}}`},
		{"with no decision", `{"reason":"hm","standing":{"route":"GET /a","seconds":60}}`},
		{"with no route", `{"allow":true,"reason":"ok","standing":{"seconds":60}}`},
		{"with no seconds", `{"allow":true,"reason":"ok","standing":{"route":"GET /a"}}`},
		{"for no time", `{"allow":true,"reason":"ok","standing":{"route":"GET /a","seconds":0}}`},
		{"for a fraction", `{"allow":true,"reason":"ok","standing":{"route":"GET /a","seconds":1.5}}`},
		{"with a field nobody defined", `{"allow":true,"reason":"ok","standing":{"route":"GET /a","seconds":60,"host":"evil.example"}}`},
		{"with the route said twice", `{"allow":true,"reason":"ok","standing":{"route":"GET /a","Route":"DELETE /a","seconds":60}}`},
		{"with a route that does not parse", `{"allow":true,"reason":"ok","standing":{"route":"/a","seconds":60}}`},
		{"as a string", `{"allow":true,"reason":"ok","standing":"GET /a"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if answer, err := judge.Decode([]byte(tc.output)); err == nil {
				t.Fatalf("Decode(%s) = %+v, want an error", tc.output, answer)
			}
		})
	}
}

func TestAStandingAllowIsCapped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		seconds int
		want    time.Duration
	}{
		{60, time.Minute},
		{900, judge.MaxStanding},
		{86400, judge.MaxStanding},
		{0, 0},
		{-5, 0},
		// Wraps to 0.29s as a Duration, which would pass a cap checked after.
		{18446744074, judge.MaxStanding},
	} {
		if got := (judge.Standing{Route: "GET /a", Seconds: tc.seconds}).Duration(); got != tc.want {
			t.Fatalf("Duration(%d) = %v, want %v", tc.seconds, got, tc.want)
		}
	}
}

// A route is the subset of net/http's pattern syntax that names a method and
// a path exactly. Anything that would widen it — a host, any method, a
// prefix, a wildcard inside a segment — is refused rather than guessed at.
func TestParseRouteAcceptsOnlyAMethodAndAnExactPath(t *testing.T) {
	t.Parallel()
	for _, pattern := range []string{
		"GET /",
		"GET /repos/org/repo",
		"GET /repos/org/repo/",
		"POST /repos/org/repo/pulls/{number}/comments",
		"GET /repos/{owner}/{repo_2}/contents/{path...}",
		"GET /a%20b",
	} {
		if _, err := judge.ParseRoute(pattern); err != nil {
			t.Fatalf("ParseRoute(%q) error = %v", pattern, err)
		}
	}
	for _, pattern := range []string{
		"",
		"/repos/org/repo",
		"get /repos",
		"GET  /repos",
		"GET api.github.com/repos",
		"GET repos",
		"GET /repos//org",
		"GET /repos/{rest...}/pulls",
		"GET /repos/{}",
		"GET /repos/{1st}",
		"GET /repos/pre{fix}",
		"GET /repos/{$}",
		"GET /repos/../admin",
		"GET /repos/%zz",
	} {
		if route, err := judge.ParseRoute(pattern); err == nil {
			t.Fatalf("ParseRoute(%q) = %+v, want an error", pattern, route)
		}
	}
}

func TestARouteMatchesItsMethodAndPathOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pattern, method, url string
		want                 bool
	}{
		{"GET /repos/org/repo", "GET", "https://api.github.com/repos/org/repo", true},
		{"GET /repos/org/repo", "GET", "https://api.github.com/repos/org/repo?per_page=100", true},
		{"GET /repos/org/repo", "HEAD", "https://api.github.com/repos/org/repo", false},
		{"GET /repos/org/repo", "GET", "https://api.github.com/repos/org/repo/", false},
		{"GET /repos/org/repo", "GET", "https://api.github.com/repos/org/repo/pulls", false},
		{"GET /repos/org/repo/", "GET", "https://api.github.com/repos/org/repo/", true},
		{"GET /repos/org/repo/", "GET", "https://api.github.com/repos/org/repo/pulls", false},
		{"GET /repos/org/{repo}", "GET", "https://api.github.com/repos/org/other", true},
		{"GET /repos/org/{repo}", "GET", "https://api.github.com/repos/org/", false},
		{"GET /repos/org/{repo}", "GET", "https://api.github.com/repos/org/a/b", false},
		// An encoded slash or backslash is a separator to many upstreams.
		{"GET /repos/org/{repo}", "GET", "https://api.github.com/repos/org/a%2Fb", false},
		{"GET /repos/org/{repo}", "GET", "https://api.github.com/repos/org/..%2F..%2Fuser%2Fkeys", false},
		{"GET /repos/org/{repo}", "GET", "https://api.github.com/repos/org/..%5C..%5Cuser", false},
		{"GET /repos/org/repo/{rest...}", "GET", "https://api.github.com/repos/org/repo/a%2F..%2F..%2Fx", false},
		{"GET /repos/org/repo/{rest...}", "GET", "https://api.github.com/repos/org/repo/pulls/1", true},
		{"GET /repos/org/repo/{rest...}", "GET", "https://api.github.com/repos/org/repo/", true},
		{"GET /repos/org/repo/{rest...}", "GET", "https://api.github.com/repos/org/repo", false},
		{"GET /repos/org/repo/{rest...}", "GET", "https://api.github.com/repos/org/repo/../../other/repo", false},
		{"GET /repos/org/repo/{rest...}", "GET", "https://api.github.com/repos/org/repo/%2e%2e/%2e%2e/other", false},
		{"GET /repos/org/repo/{rest...}", "GET", "https://api.github.com/repos/org/repo/./pulls", false},
		{"GET /repos/org/repo/{rest...}", "GET", "https://api.github.com/repos/org/repo//pulls", false},
		{"GET /a%20b", "GET", "https://example.com/a%20b", true},
		{"GET /a b", "GET", "https://example.com/a%20b", true},
		{"GET /", "GET", "https://example.com", false},
	} {
		route, err := judge.ParseRoute(tc.pattern)
		if err != nil {
			t.Fatalf("ParseRoute(%q) error = %v", tc.pattern, err)
		}
		if got := route.Matches(tc.method, tc.url); got != tc.want {
			t.Fatalf("%q Matches(%s %s) = %v, want %v", tc.pattern, tc.method, tc.url, got, tc.want)
		}
	}
}

// Whether an allow stands is Discobox's to decide: only a first-round allow,
// decided without the body, whose route covers the request it was granted on.
func TestAJobAdmitsOnlyARouteDerivedFromItsOwnEvidence(t *testing.T) {
	t.Parallel()
	request := func(round int, body *judge.Body) judge.Job {
		return judge.Job{
			Kind: judge.KindRequest, Purpose: "open a PR in org/repo", Host: "api.github.com", Round: round,
			Request: &judge.Request{Method: "GET", URL: "https://api.github.com/repos/org/repo/pulls?page=2", Body: body},
		}
	}
	standing := judge.Standing{Route: "GET /repos/org/repo/pulls", Seconds: 300}
	if _, err := request(1, nil).Admits(standing); err != nil {
		t.Fatalf("Admits() error = %v", err)
	}
	if _, err := request(1, &judge.Body{MediaType: "application/json", Length: 10}).Admits(standing); err != nil {
		t.Fatalf("a described, unshown body refused the route: %v", err)
	}
	if _, err := request(2, &judge.Body{Length: 10, Form: judge.FormJSON, Content: "{}"}).Admits(standing); err == nil {
		t.Fatal("an allow that needed the body was let stand")
	}
	if _, err := request(1, nil).Admits(judge.Standing{Route: "GET /repos/org/other/pulls", Seconds: 300}); err == nil {
		t.Fatal("a route that does not cover its own request was let stand")
	}
	if _, err := request(1, nil).Admits(judge.Standing{Route: "POST /repos/org/repo/pulls", Seconds: 300}); err == nil {
		t.Fatal("a route for another method was let stand")
	}
	if _, err := request(1, nil).Admits(judge.Standing{Route: "GET /repos/org/repo/pulls", Seconds: 0}); err == nil {
		t.Fatal("a route standing for no time was let stand")
	}
	for _, route := range []string{"GET /{rest...}", "GET /{a}/{b}/{c}/{d...}"} {
		if _, err := request(1, nil).Admits(judge.Standing{Route: route, Seconds: 300}); err == nil {
			t.Fatalf("%q, which names no target, was let stand", route)
		}
	}
	command := judge.Job{Kind: judge.KindCommand, Purpose: "p", Host: "h", Round: 1, Command: []string{"gh"}}
	if _, err := command.Admits(standing); err == nil {
		t.Fatal("a command's allow was let stand")
	}
}
