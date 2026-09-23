package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pool-agent/poolauth"
)

// Asking for a verdict is the credential broker's authority, and a pool that
// holds it still gets no verdict where there is no judge — which is the
// answer, not an error to route around (ADR 0141 §1).
func TestJudgeRouteIsTheBrokersAndAnswersNothingWithoutAJudge(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)
	router := newTestApp(ctx, t, db)
	projectID, privateKey := seedCredentialRoutePool(ctx, t, db.Write, router)

	job := `{"kind":"request","purpose":"open a pull request in org/repo","host":"api.github.com","round":1,` +
		`"request":{"method":"POST","url":"https://api.github.com/repos/org/repo/pulls"}}`
	path := "/api/pools/" + routeTestPoolID + "/judge"

	for _, tc := range []struct {
		name  string
		token string
		want  int
	}{
		{"the broker's own scope", signPoolAssertion(t, projectID, routeTestPoolID, privateKey, poolauth.ScopeCredentialBroker),
			http.StatusServiceUnavailable},
		{"a pool that may only resolve sentinels", signPoolAssertion(t, projectID, routeTestPoolID, privateKey, poolauth.ScopeSecretResolve),
			http.StatusForbidden},
		{"a pool that may forward its discoboxes' calls", signPoolAssertion(t, projectID, routeTestPoolID, privateKey, poolauth.ScopeSandboxForward),
			http.StatusForbidden},
		{"an assertion with no scope at all", signPoolAssertion(t, projectID, routeTestPoolID, privateKey), http.StatusForbidden},
		{"nothing", "", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, strings.NewReader(job))
			req.Header.Set("Content-Type", "application/json")
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			router.ServeHTTP(resp, req)
			if resp.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", resp.Code, tc.want, resp.Body.String())
			}
			if resp.Code == http.StatusServiceUnavailable && !strings.Contains(resp.Body.String(), "no judge") {
				t.Fatalf("body = %s, want it to say the project has no judge and why", resp.Body.String())
			}
		})
	}
}

// A job that could not be judged is refused before a judge is looked for: the
// shape of a job is the trusted side's own doing, and a malformed one is a
// caller that would be asking about evidence nobody sent.
func TestJudgeRouteRefusesAJobItCannotJudge(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)
	router := newTestApp(ctx, t, db)
	projectID, privateKey := seedCredentialRoutePool(ctx, t, db.Write, router)
	token := signPoolAssertion(t, projectID, routeTestPoolID, privateKey, poolauth.ScopeCredentialBroker)

	for _, tc := range []struct{ name, job string }{
		{"no approved use", `{"kind":"request","purpose":"","host":"api.github.com","round":1,"request":{"method":"GET","url":"https://api.github.com/"}}`},
		{"a kind nobody defined", `{"kind":"terminal","purpose":"open a PR","host":"api.github.com","round":1}`},
		{"a first ask carrying the body", `{"kind":"request","purpose":"open a PR","host":"api.github.com","round":1,` +
			`"request":{"method":"POST","url":"https://api.github.com/graphql","body":{"form":"json","content":"{}"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/pools/"+routeTestPoolID+"/judge", strings.NewReader(tc.job))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			router.ServeHTTP(resp, req)
			if resp.Code != http.StatusBadRequest && resp.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want the job refused; body = %s", resp.Code, resp.Body.String())
			}
			var answered map[string]any
			_ = json.Unmarshal(resp.Body.Bytes(), &answered)
			if allow, ok := answered["allow"]; ok && allow == true {
				t.Fatalf("body = %s, want nothing that reads as an allow", resp.Body.String())
			}
		})
	}
}
