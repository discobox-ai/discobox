package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/poolauth"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/resources/judges"
	"github.com/go-chi/chi/v5"
)

// Asking for a verdict is the credential broker's authority, and a pool that
// holds it still gets no verdict where there is no judge — which is the
// answer, not an error to route around (ADR 26-09-22-838 §1).
func TestJudgeRouteIsTheBrokersAndAnswersNothingWithoutAJudge(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)
	router := newJudgingTestApp(ctx, t, db)
	projectID, privateKey := seedCredentialRoutePool(ctx, t, db.Write, router)

	job := `{"sandboxId":"sb_notthere","useId":"use_abc","round":1,` +
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

// An ask that is not one is refused on the wire. What a pool may say is only
// which discobox is spending which use and what its proxy saw; an ask missing
// either of those names nothing that could be judged. What the use approves is
// not a field it has, so it cannot be malformed here — the rules about the
// evidence itself are the service's, and are tested there.
func TestJudgeRouteRefusesAnAskThatIsNotOne(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)
	router := newJudgingTestApp(ctx, t, db)
	projectID, privateKey := seedCredentialRoutePool(ctx, t, db.Write, router)
	token := signPoolAssertion(t, projectID, routeTestPoolID, privateKey, poolauth.ScopeCredentialBroker)

	for _, tc := range []struct{ name, job string }{
		{"no discobox", `{"useId":"use_abc","round":1,"request":{"method":"GET","url":"https://api.github.com/"}}`},
		{"no use", `{"sandboxId":"sb_1","round":1,"request":{"method":"GET","url":"https://api.github.com/"}}`},
		{"nothing observed", `{"sandboxId":"sb_1","useId":"use_abc","round":1}`},
		{"a purpose of its own", `{"sandboxId":"sb_1","useId":"use_abc","round":1,"purpose":"anything at all",` +
			`"request":{"method":"GET","url":"https://api.github.com/"}}`},
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

// newJudgingTestApp is an app that judges. The route is off by default, and a
// server that has not opted in refuses every ask before looking for a judge —
// which is its own test, in the judges package.
func newJudgingTestApp(ctx context.Context, t *testing.T, db *database.DB) *chi.Mux {
	t.Helper()
	opts := DefaultAppOptions()
	opts.Judging = judges.Judging{Commands: true, Requests: true}
	router, _, _, stop, err := NewApp(ctx, db.Write, db.Read, opts)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := stop(stopCtx); err != nil {
			t.Errorf("stop services: %v", err)
		}
	})
	return router
}

// A server that does not judge says so in a way a program can act on: a pool
// has to tell "stop asking" apart from "the judge failed", and it cannot do
// that by reading a sentence written for a person.
func TestJudgeRouteSaysWhenItDoesNotJudgeInAWayAProgramReads(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)
	// The ordinary app: judging is off unless a server opted in.
	router := newTestApp(ctx, t, db)
	projectID, privateKey := seedCredentialRoutePool(ctx, t, db.Write, router)
	token := signPoolAssertion(t, projectID, routeTestPoolID, privateKey, poolauth.ScopeCredentialBroker)

	body := `{"sandboxId":"sb_1","useId":"use_abc","round":1,` +
		`"request":{"method":"GET","url":"https://api.github.com/"}}`
	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/pools/"+routeTestPoolID+"/judge", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", resp.Code, resp.Body.String())
	}
	var problem map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &problem); err != nil {
		t.Fatalf("body is not a problem document: %v; body = %s", err, resp.Body.String())
	}
	if problem["type"] != "urn:discobox:problem:judging-disabled" {
		t.Fatalf("type = %v, want the kind a pool recognizes; body = %s", problem["type"], resp.Body.String())
	}
}

// The command route is a pool route like the request one: a real pool's
// assertion reaches the handler (server/internal/auth/REVIEW.md), and a server
// that does not judge commands answers in the way the pool mints by
// (ADR 26-10-02-054 §3).
func TestJudgeCommandsRouteIsAPoolRoute(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	body := `{"sandboxId":"sb_1","useId":"use_abc","command":["gh","pr","create"],"stdin":{"content":"x"}}`
	ask := func(t *testing.T, router http.Handler, token string) *httptest.ResponseRecorder {
		t.Helper()
		resp := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/pools/"+routeTestPoolID+"/judge-commands", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		router.ServeHTTP(resp, req)
		return resp
	}

	t.Run("a server that does not judge commands", func(t *testing.T) {
		db := newAppTestDB(ctx, t)
		router := newTestApp(ctx, t, db)
		projectID, privateKey := seedCredentialRoutePool(ctx, t, db.Write, router)
		resp := ask(t, router, signPoolAssertion(t, projectID, routeTestPoolID, privateKey, poolauth.ScopeCredentialBroker))
		var problem map[string]any
		_ = json.Unmarshal(resp.Body.Bytes(), &problem)
		if resp.Code != http.StatusServiceUnavailable || problem["type"] != "urn:discobox:problem:judging-disabled" {
			t.Fatalf("status = %d, body = %s; want the judging-disabled problem", resp.Code, resp.Body.String())
		}
	})
	t.Run("a server that judges commands", func(t *testing.T) {
		db := newAppTestDB(ctx, t)
		router := newJudgingTestApp(ctx, t, db)
		projectID, privateKey := seedCredentialRoutePool(ctx, t, db.Write, router)
		resp := ask(t, router, signPoolAssertion(t, projectID, routeTestPoolID, privateKey, poolauth.ScopeCredentialBroker))
		// The project has no judge, so it is refused — by the judge's
		// service, not by authentication.
		if resp.Code == http.StatusUnauthorized || resp.Code == http.StatusForbidden || !strings.Contains(resp.Body.String(), "judge") {
			t.Fatalf("status = %d, body = %s; want the pool through to the judge service", resp.Code, resp.Body.String())
		}
	})
}
