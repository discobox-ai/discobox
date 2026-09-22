package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/poolauth"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// A sandbox's own calls, as its pool forwards them from the reserved host
// (ADR 0140), exercised through the real router: the forwarded-sandbox
// authenticator, the sandbox role, and everything behind them.

// forwardRoute calls router as the pool poolID forwarding a call from
// sandboxID, with the pool's assertion token.
func forwardRoute(t *testing.T, router http.Handler, method, path, token, poolID, sandboxID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(""))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set(poolauth.ForwardingPoolHeader, poolID)
	req.Header.Set(poolauth.ForwardedSandboxHeader, sandboxID)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

// The sandbox role is a fixed list of routes in the sandbox's own project, and
// a forwarded call outside it is refused whatever else would admit it — in
// particular the routes any authenticated caller reaches, like enrolling a
// peer or registering a pool.
func TestASandboxHoldsOnlyTheSandboxRole(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)
	router := newTestApp(ctx, t, db)
	projectID, key := seedCredentialRoutePool(ctx, t, db.Write, router)
	token := signPoolAssertion(t, projectID, routeTestPoolID, key, poolauth.ScopeSandboxForward)
	project := "/projects/" + projectID

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, project + "/sandboxes", http.StatusOK},
		{http.MethodGet, "/projects/default/sandboxes", http.StatusOK},
		{http.MethodGet, project + "/sandboxes/" + routeTestSandboxID, http.StatusOK},
		{http.MethodGet, project + "/secrets", http.StatusOK},
		{http.MethodGet, project + "/secret-requests", http.StatusOK},

		{http.MethodDelete, project + "/sandboxes/" + routeTestSandboxID, http.StatusForbidden},
		{http.MethodPost, project + "/secrets", http.StatusForbidden},
		{http.MethodGet, project + "/secret-grants", http.StatusForbidden},
		{http.MethodGet, project + "/pools", http.StatusForbidden},
		{http.MethodGet, "/projects/another-project/sandboxes", http.StatusForbidden},
		{http.MethodGet, "/projects", http.StatusForbidden},
		{http.MethodGet, "/peers", http.StatusForbidden},
		{http.MethodPost, "/api/pools/register", http.StatusForbidden},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			resp := forwardRoute(t, router, tc.method, tc.path, token, routeTestPoolID, routeTestSandboxID)
			if resp.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", resp.Code, tc.want, resp.Body.String())
			}
		})
	}
}

// A forwarded call is the pool's word for which sandbox is calling, and it is
// taken only when that word is good. Anything less is refused outright: the
// authenticator after it answers every request as the default user, so a
// forwarded call must never reach it.
func TestAForwardedCallIsTheSandboxOnlyOnItsPoolsWord(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)
	router := newTestApp(ctx, t, db)
	projectID, key := seedCredentialRoutePool(ctx, t, db.Write, router)
	otherKey := seedPool(ctx, t, db.Write, projectID, "pool-other")
	revokedKey := seedPool(ctx, t, db.Write, projectID, "pool-revoked")
	revokedAt := time.Now().UTC()
	if err := db.Write.WithContext(ctx).Model(&model.Pool{}).Where("id = ?", "pool-revoked").Update("revoked_at", revokedAt).Error; err != nil {
		t.Fatalf("revoke pool: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&model.Sandbox{
		ID: "sbx-revoked", ProjectID: projectID, Name: "revoked", PoolID: "pool-revoked",
	}).Error; err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	path := "/projects/" + projectID + "/sandboxes"

	for _, tc := range []struct {
		name, token, poolID, sandboxID string
	}{
		{"no assertion", "", routeTestPoolID, routeTestSandboxID},
		{"an assertion without the forwarding scope",
			signPoolAssertion(t, projectID, routeTestPoolID, key, poolauth.ScopeCredentialBroker), routeTestPoolID, routeTestSandboxID},
		{"another pool speaking for this pool's sandbox",
			signPoolAssertion(t, projectID, "pool-other", otherKey, poolauth.ScopeSandboxForward), "pool-other", routeTestSandboxID},
		{"an assertion claiming a pool it was not signed by",
			signPoolAssertion(t, projectID, "pool-other", otherKey, poolauth.ScopeSandboxForward), routeTestPoolID, routeTestSandboxID},
		{"a revoked pool",
			signPoolAssertion(t, projectID, "pool-revoked", revokedKey, poolauth.ScopeSandboxForward), "pool-revoked", "sbx-revoked"},
		{"a sandbox that does not exist",
			signPoolAssertion(t, projectID, routeTestPoolID, key, poolauth.ScopeSandboxForward), routeTestPoolID, "sbx-nobody"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := forwardRoute(t, router, http.MethodGet, path, tc.token, tc.poolID, tc.sandboxID)
			if resp.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", resp.Code, resp.Body.String())
			}
		})
	}
}
