package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/pprofserve"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// newPprofRouter is the agent's router on a pool whose processes serve their
// profiles at targets; none is a pool with profiling off.
func newPprofRouter(t *testing.T, targets map[string]PprofTarget) (http.Handler, func(projectID, poolID, sandboxID string, scopes ...string) string) {
	t.Helper()
	publicKey, sign := testPoolTokenSigner(t)
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: "project-1", PoolID: "pool-1"},
		Runtime:               sandboxruntime.NewMemorySandboxRuntime(),
		ControlPlanePublicKey: publicKey,
		Pprof:                 targets,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	return router, sign
}

func pprofRequest(router http.Handler, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/project/project-1/pool/pool-1/pprof/"+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

// startProcessPprof is one of the pool's processes serving its profiles, as
// pprofserve does in the agent, the proxy and the mediator.
func startProcessPprof(t *testing.T) PprofTarget {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "pprof-proxy.token")
	bound, stop, err := pprofserve.Start(context.Background(), pprofserve.Config{Setting: "test", Addr: "127.0.0.1:0", TokenFile: tokenFile})
	if err != nil {
		t.Fatalf("start pprof: %v", err)
	}
	t.Cleanup(stop)
	return PprofTarget{Address: bound.String(), TokenFile: tokenFile}
}

// The relay reads the process's token and fetches the named profile from its
// listener, unchanged.
func TestPprofRelayFetchesTheProcessesProfile(t *testing.T) {
	router, sign := newPprofRouter(t, map[string]PprofTarget{"proxy": startProcessPprof(t)})
	resp := pprofRequest(router, "proxy/goroutine", sign("project-1", "pool-1", "", ScopePoolPprof))
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s; want the profile", resp.Code, resp.Body.String())
	}
	if resp.Body.Len() == 0 || resp.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("relayed %d bytes of %q, want a binary profile", resp.Body.Len(), resp.Header().Get("Content-Type"))
	}
	resp = pprofRequest(router, "proxy/profile?seconds=1", sign("project-1", "pool-1", "", ScopePoolPprof))
	if resp.Code != http.StatusOK || resp.Body.Len() == 0 {
		t.Fatalf("CPU profile status = %d, %d bytes, want a profile", resp.Code, resp.Body.Len())
	}
}

func TestPprofRelayRefusals(t *testing.T) {
	running := startProcessPprof(t)
	var listenConfig net.ListenConfig
	closed, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddress := closed.Addr().String()
	_ = closed.Close()
	targets := map[string]PprofTarget{
		"agent": running,
		// Its process is not serving: it wrote no token.
		"proxy": {Address: running.Address, TokenFile: filepath.Join(t.TempDir(), "absent.token")},
		// Its token is there, and nothing listens.
		"mediator": {Address: closedAddress, TokenFile: running.TokenFile},
	}
	for _, tc := range []struct {
		name       string
		targets    map[string]PprofTarget
		path       string
		scopes     []string
		wantStatus int
		wantDetail string
	}{
		{name: "no pool:pprof", targets: targets, path: "agent/heap", scopes: []string{ScopeSandboxRead, ScopeAuditRead}, wantStatus: http.StatusForbidden},
		{name: "profiling off", path: "agent/heap", scopes: []string{ScopePoolPprof}, wantStatus: http.StatusNotFound, wantDetail: "profiling is off for pool pool-1"},
		{name: "no such process", targets: targets, path: "buildkitd/heap", scopes: []string{ScopePoolPprof}, wantStatus: http.StatusNotFound, wantDetail: "no process"},
		{name: "not a profile", targets: targets, path: "agent/cmdline", scopes: []string{ScopePoolPprof}, wantStatus: http.StatusNotFound, wantDetail: "no profile"},
		{name: "seconds not a number", targets: targets, path: "agent/profile?seconds=soon", scopes: []string{ScopePoolPprof}, wantStatus: http.StatusBadRequest},
		{name: "process not serving", targets: targets, path: "proxy/heap", scopes: []string{ScopePoolPprof}, wantStatus: http.StatusBadGateway, wantDetail: "not serving profiles"},
		{name: "process not answering", targets: targets, path: "mediator/heap", scopes: []string{ScopePoolPprof}, wantStatus: http.StatusBadGateway, wantDetail: "does not answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, sign := newPprofRouter(t, tc.targets)
			resp := pprofRequest(router, tc.path, sign("project-1", "pool-1", "", tc.scopes...))
			if resp.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", resp.Code, tc.wantStatus, resp.Body.String())
			}
			// Every refusal is the relay's own problem+json, never the
			// router's text/plain 404 an agent without the relay answers.
			if got := resp.Header().Get("Content-Type"); got != "application/problem+json" {
				t.Fatalf("content type = %q, want application/problem+json", got)
			}
			if !strings.Contains(resp.Body.String(), tc.wantDetail) {
				t.Fatalf("body = %s, want it to say %q", resp.Body.String(), tc.wantDetail)
			}
		})
	}
}

// A token for another pool is refused before the relay, as on every route.
func TestPprofRelayRefusesAnotherPoolsToken(t *testing.T) {
	router, sign := newPprofRouter(t, map[string]PprofTarget{"agent": startProcessPprof(t)})
	resp := pprofRequest(router, "agent/heap", sign("project-1", "pool-2", "", ScopePoolPprof))
	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", resp.Code, resp.Body.String())
	}
}
