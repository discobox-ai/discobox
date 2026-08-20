package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// Every sandbox-agent path this router forwards is named here, so a route the
// control plane and the sandbox-agent both know still 404s in the middle until
// it is registered. That is what the service routes did.
func TestSandboxServiceProxyForwardsEveryRoute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		path   string
		want   string
		scope  string
	}{
		{"list", http.MethodGet, "/services", "/api/projects/project-1/sandboxes/sandbox-1/services", ScopeExecRead},
		{"get", http.MethodGet, "/services/api", "/api/projects/project-1/sandboxes/sandbox-1/services/api", ScopeExecRead},
		{"logs", http.MethodGet, "/services/api/logs", "/api/projects/project-1/sandboxes/sandbox-1/services/api/logs", ScopeExecRead},
		{"start", http.MethodPost, "/services/api/start", "/api/projects/project-1/sandboxes/sandbox-1/services/api/start", ScopeExecWrite},
		{"stop", http.MethodPost, "/services/api/stop", "/api/projects/project-1/sandboxes/sandbox-1/services/api/stop", ScopeExecWrite},
		{"restart", http.MethodPost, "/services/api/restart", "/api/projects/project-1/sandboxes/sandbox-1/services/api/restart", ScopeExecWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, sign := newServiceProxyRouter(t, tc.want)
			req := httptest.NewRequestWithContext(context.Background(), tc.method,
				"/api/project/project-1/pool/pool-1/sandboxes/sandbox-1"+tc.path, nil)
			req.Header.Set("Authorization", "Bearer "+sign("project-1", "pool-1", "sandbox-1", tc.scope))
			req.Header.Set(sandboxAgentAuthorizationHeader, "Bearer sandbox-token")
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)

			if resp.Code != http.StatusOK {
				t.Fatalf("proxy status = %d, body = %s", resp.Code, resp.Body.String())
			}
		})
	}
}

// A service is an exec, so the exec scopes gate it: reading one without
// exec:read, or acting on one with only exec:read, is refused here rather than
// inside the sandbox.
func TestSandboxServiceProxyRequiresExecScopes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		path   string
		scope  string
	}{
		{"list without exec:read", http.MethodGet, "/services", ScopeSandboxHTTP},
		{"start with only exec:read", http.MethodPost, "/services/api/start", ScopeExecRead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, sign := newServiceProxyRouter(t, "")
			req := httptest.NewRequestWithContext(context.Background(), tc.method,
				"/api/project/project-1/pool/pool-1/sandboxes/sandbox-1"+tc.path, nil)
			req.Header.Set("Authorization", "Bearer "+sign("project-1", "pool-1", "sandbox-1", tc.scope))
			req.Header.Set(sandboxAgentAuthorizationHeader, "Bearer sandbox-token")
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)

			if resp.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body = %s", resp.Code, resp.Body.String())
			}
		})
	}
}

// newServiceProxyRouter builds a router in front of an upstream that asserts
// the path it is reached on. wantPath empty means the upstream must not be
// reached at all.
func newServiceProxyRouter(t *testing.T, wantPath string) (http.Handler, func(string, string, string, ...string) string) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantPath == "" {
			t.Errorf("upstream reached at %q; the request should have been refused", r.URL.Path)
			return
		}
		if r.URL.Path != wantPath {
			t.Errorf("upstream path = %q, want %q", r.URL.Path, wantPath)
		}
		_, _ = w.Write([]byte(`{"services":[]}`))
	}))
	t.Cleanup(upstream.Close)
	baseURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, sign := testPoolTokenSigner(t)
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: "project-1", PoolID: "pool-1"},
		Runtime:               proxyTestRuntime{MemorySandboxRuntime: sandboxruntime.NewMemorySandboxRuntime(), baseURL: baseURL},
		ControlPlanePublicKey: publicKey,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	return router, sign
}
