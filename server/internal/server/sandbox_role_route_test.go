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
// sandboxID, with the pool's assertion token. A body is sent as JSON.
func forwardRoute(t *testing.T, router http.Handler, method, path, body, token, poolID, sandboxID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
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
		// An approval rule is a person's: a discobox never lists, reads,
		// makes or edits one, and asks for no proposals (ADR 26-10-12-330 §1).
		{http.MethodGet, project + "/approval-rules", http.StatusForbidden},
		{http.MethodPost, project + "/approval-rules", http.StatusForbidden},
		{http.MethodGet, project + "/approval-rules/rule_1", http.StatusForbidden},
		{http.MethodPatch, project + "/approval-rules/rule_1", http.StatusForbidden},
		{http.MethodDelete, project + "/approval-rules/rule_1", http.StatusForbidden},
		{http.MethodPost, project + "/secret-requests/sreq_1/rule-proposals", http.StatusForbidden},
		{http.MethodPost, project + "/secret-grants/grant_1/rule-proposals", http.StatusForbidden},
		{http.MethodGet, project + "/pools", http.StatusForbidden},
		{http.MethodGet, "/projects/another-project/sandboxes", http.StatusForbidden},
		{http.MethodGet, "/projects", http.StatusForbidden},
		{http.MethodGet, "/peers", http.StatusForbidden},
		{http.MethodPost, "/api/pools/register", http.StatusForbidden},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			resp := forwardRoute(t, router, tc.method, tc.path, "", token, routeTestPoolID, routeTestSandboxID)
			if resp.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", resp.Code, tc.want, resp.Body.String())
			}
		})
	}
}

// A discobox starts, stops, restarts and repairs any discobox in its project,
// and the call gets past the role and the handler into the sandbox service:
// neither refuses a sandbox caller (ADR 26-10-02-478 §1, ADR 26-10-14-320
// §§1, 3). Both discoboxes are archived, so the service answers with its own
// refusal before it resolves a provider or instructs the pool; what lies past
// that point reads no principal, and is not reached here. The project's judge
// is refused before the service is asked (ADR 26-10-14-320 §4).
func TestASandboxPowersAnyDiscoboxInItsProjectButTheJudge(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)
	router := newTestApp(ctx, t, db)
	projectID, key := seedCredentialRoutePool(ctx, t, db.Write, router)
	token := signPoolAssertion(t, projectID, routeTestPoolID, key, poolauth.ScopeSandboxForward)
	lead := routeTestSandboxID
	archived := model.ResourceLifecycle{DesiredState: model.DesiredStateArchived}
	for _, sandbox := range []*model.Sandbox{
		{ID: "sbx-worker", Name: "worker", CreatedBySandboxID: &lead, ResourceLifecycle: archived},
		{ID: "sbx-persons", Name: "persons", ResourceLifecycle: archived},
		{ID: "sbx-judge", Name: "judge"},
	} {
		sandbox.ProjectID, sandbox.PoolID = projectID, routeTestPoolID
		if err := db.Write.WithContext(ctx).Create(sandbox).Error; err != nil {
			t.Fatalf("create sandbox %s: %v", sandbox.ID, err)
		}
	}
	if err := db.Write.WithContext(ctx).Model(&model.Project{}).Where("id = ?", projectID).Update("judge_sandbox_id", "sbx-judge").Error; err != nil {
		t.Fatalf("name the project's judge: %v", err)
	}

	for _, action := range []string{"start", "stop", "restart", "repair"} {
		for _, target := range []string{"sbx-worker", "sbx-persons"} {
			t.Run(action+" "+target, func(t *testing.T) {
				resp := forwardRoute(t, router, http.MethodPost, "/projects/default/sandboxes/"+target+"/"+action, "{}", token, routeTestPoolID, lead)
				if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "archived") {
					t.Fatalf("status = %d, want the service's 409 for an archived discobox; body = %s", resp.Code, resp.Body.String())
				}
			})
		}
		t.Run(action+" the judge", func(t *testing.T) {
			resp := forwardRoute(t, router, http.MethodPost, "/projects/default/sandboxes/sbx-judge/"+action, "{}", token, routeTestPoolID, lead)
			if resp.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body = %s", resp.Code, resp.Body.String())
			}
		})
	}
}

// A discobox changes the meta of any discobox in its project, and the call
// gets past the role and the handler into the sandbox service: neither refuses
// a sandbox caller (ADR 26-10-08-447 §1, ADR 26-10-14-320 §1). The change both
// sets and removes one tag, so the service refuses it as the caller's mistake
// before it starts the discobox or leases a client to it; what lies past that
// point is the scope check authorizeRequestedScopes already admits, and is not
// reached here.
func TestASandboxTagsAnyDiscoboxInItsProject(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)
	router := newTestApp(ctx, t, db)
	projectID, key := seedCredentialRoutePool(ctx, t, db.Write, router)
	token := signPoolAssertion(t, projectID, routeTestPoolID, key, poolauth.ScopeSandboxForward)
	lead := routeTestSandboxID
	for _, sandbox := range []*model.Sandbox{
		{ID: "sbx-worker", Name: "worker", CreatedBySandboxID: &lead},
		{ID: "sbx-persons", Name: "persons"},
	} {
		sandbox.ProjectID, sandbox.PoolID = projectID, routeTestPoolID
		if err := db.Write.WithContext(ctx).Create(sandbox).Error; err != nil {
			t.Fatalf("create sandbox %s: %v", sandbox.ID, err)
		}
	}
	const body = `{"setTags":{"to-delete":""},"removeTags":["to-delete"]}`

	for _, target := range []string{"sbx-worker", "sbx-persons"} {
		t.Run(target, func(t *testing.T) {
			resp := forwardRoute(t, router, http.MethodPatch, "/projects/default/sandboxes/"+target+"/meta", body, token, routeTestPoolID, lead)
			if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "both set and removed") {
				t.Fatalf("status = %d, want the service's 400 for the change; body = %s", resp.Code, resp.Body.String())
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
			resp := forwardRoute(t, router, http.MethodGet, path, "", tc.token, tc.poolID, tc.sandboxID)
			if resp.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", resp.Code, resp.Body.String())
			}
		})
	}
}
