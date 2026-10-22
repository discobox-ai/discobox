package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/service"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

// The files API is forwarded like the exec routes, on scopes of its own: a
// stat, a listing and a read are reads, which start a stopped sandbox and
// repair nothing, and every write is an action (ADR 26-10-22-499 §3). The
// path and the query reach the pool agent as they came.
func TestSandboxFileRoutesNameTheirUseAndScope(t *testing.T) {
	stubs := newRouterTestServices()
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID: "sandbox-1", ProjectID: testDefaultProjectID,
		CreatedByUserID: service.DefaultUserID, PoolID: "pool-1",
	}
	var mu sync.Mutex
	var upstreamPath, upstreamQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every port that starts listening in a sandbox is probed once
		// (sandbox-agent/ports/probe.go), and this test may run in one.
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			return
		}
		mu.Lock()
		upstreamPath, upstreamQuery = r.URL.Path, r.URL.RawQuery
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	stubs.sandboxLease = transport.NewHTTPClientLeaseWithBaseURLAndAuth(upstream.Client(), upstream.URL, "worker-token", nil)
	router, err := NewRouter(services.Services{
		Projects: stubs, HarnessConfigs: stubs, Sandboxes: stubs,
		Providers: stubs, Pools: stubs, Jobs: stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	base := "/api/projects/" + testDefaultProjectID + "/sandboxes/sandbox-1"
	read, write := []string{poolagentauth.ScopeFilesRead}, []string{poolagentauth.ScopeFilesWrite}
	for _, tc := range []struct {
		method, path string
		use          services.SandboxUse
		scopes       []string
	}{
		{http.MethodGet, "/files/stat?path=%2Fetc%2Fhosts", services.SandboxRead, read},
		{http.MethodGet, "/files/list?path=src", services.SandboxRead, read},
		{http.MethodGet, "/files/content?path=a&offset=2", services.SandboxRead, read},
		{http.MethodPut, "/files/content?path=a&ifExists=fail", services.SandboxAction("file write"), write},
		{http.MethodPost, "/files/mkdir", services.SandboxAction("file mkdir"), write},
		{http.MethodPost, "/files/remove", services.SandboxAction("file remove"), write},
		{http.MethodPost, "/files/move", services.SandboxAction("file move"), write},
		{http.MethodPost, "/files/copy", services.SandboxAction("file copy"), write},
	} {
		stubs.sandboxUses, stubs.sandboxScopes = nil, nil
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, scopedUserRequest(context.Background(), tc.method, base+tc.path, nil, tc.scopes...))
		if resp.Code != http.StatusOK {
			t.Errorf("%s %s: status = %d, body = %s", tc.method, tc.path, resp.Code, resp.Body.String())
			continue
		}
		if len(stubs.sandboxUses) != 1 || stubs.sandboxUses[0] != tc.use {
			t.Errorf("%s %s: acquired as %+v, want %+v", tc.method, tc.path, stubs.sandboxUses, tc.use)
		}
		if !slices.Equal(stubs.sandboxScopes, tc.scopes) {
			t.Errorf("%s %s: scopes = %v, want %v", tc.method, tc.path, stubs.sandboxScopes, tc.scopes)
		}
		route, query, _ := strings.Cut(tc.path, "?")
		mu.Lock()
		upstreamPath, upstreamQuery := upstreamPath, upstreamQuery
		mu.Unlock()
		if upstreamPath != "/api/project/"+testDefaultProjectID+"/pool/pool-1/sandboxes/sandbox-1"+route || upstreamQuery != query {
			t.Errorf("%s %s: upstream %s?%s", tc.method, tc.path, upstreamPath, upstreamQuery)
		}
	}
}
