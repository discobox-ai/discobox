package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	poolagentauth "github.com/obot-platform/discobox/server/internal/auth/poolagent"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/service"
	"github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/transport"
)

func TestSandboxTCPAttachRouteProxiesToSandboxAgent(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       testDefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		PoolID:          "pool-1",
	}
	var released bool
	projectID := testDefaultProjectID
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/project/" + projectID + "/pool/pool-1/sandboxes/sandbox-1/tcp/attach"
		if r.URL.Path != wantPath {
			t.Errorf("upstream path = %q, want %q", r.URL.Path, wantPath)
		}
		if got := r.URL.Query().Get("host"); got != "127.0.0.1" {
			t.Errorf("upstream host = %q", got)
		}
		if got := r.URL.Query().Get("port"); got != "8080" {
			t.Errorf("upstream port = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer worker-token" {
			t.Errorf("upstream auth = %q", got)
		}
		_, _ = w.Write([]byte("tunnel"))
	}))
	t.Cleanup(upstream.Close)
	stubs.sandboxLease = transport.NewHTTPClientLeaseWithBaseURLAndAuth(upstream.Client(), upstream.URL, "worker-token", func() {
		released = true
	})

	router, err := NewRouter(tcpProxyTestServices(stubs))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodGet, "/api/projects/"+projectID+"/sandboxes/sandbox-1/tcp/attach?host=127.0.0.1&port=8080", nil, poolagentauth.ScopeTCPConnect)
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("GET tcp attach status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if body := resp.Body.String(); body != "tunnel" {
		t.Fatalf("body = %q, want proxied response", body)
	}
	if !released {
		t.Fatal("expected sandbox HTTP lease to be released")
	}
	if !slices.Equal(stubs.sandboxScopes, []string{poolagentauth.ScopeTCPConnect}) {
		t.Fatalf("sandbox scopes = %#v, want only %q", stubs.sandboxScopes, poolagentauth.ScopeTCPConnect)
	}
}

func TestSandboxTCPAttachRouteRejectsBadTarget(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       testDefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		PoolID:          "pool-1",
	}
	router, err := NewRouter(tcpProxyTestServices(stubs))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	for _, query := range []string{"", "?host=127.0.0.1", "?port=8080", "?host=127.0.0.1&port=0", "?host=127.0.0.1&port=99999", "?host=127.0.0.1&port=http"} {
		resp := httptest.NewRecorder()
		req := scopedUserRequest(ctx, http.MethodGet, "/api/projects/"+testDefaultProjectID+"/sandboxes/sandbox-1/tcp/attach"+query, nil, poolagentauth.ScopeTCPConnect)
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("GET tcp attach%q status = %d, want 400", query, resp.Code)
		}
	}
	if len(stubs.sandboxScopes) != 0 {
		t.Fatalf("expected no lease to be acquired for a bad target, got scopes %#v", stubs.sandboxScopes)
	}
}

func tcpProxyTestServices(stubs *routerTestServices) services.Services {
	return services.Services{
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Pools:          stubs,
		Jobs:           stubs,
		Events:         stubs,
	}
}
