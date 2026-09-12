package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/server/internal/services"
)

func newServerInfoRouterForTest(info services.ServerInfo) *chi.Mux {
	stubs := newRouterTestServices()
	router, _ := NewRouter(services.Services{
		ServerInfo:     info,
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Pools:          stubs,
		Jobs:           stubs,
	})
	return router
}

func getServerInfo(t *testing.T, router http.Handler) (int, map[string]any) {
	t.Helper()
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/server", nil))
	var body map[string]any
	if resp.Body.Len() > 0 {
		if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode GET /server body %q: %v", resp.Body.String(), err)
		}
	}
	return resp.Code, body
}

// What a client asks when it registers this server: the name to offer
// (ADR 0116 §2). Routing and the handler only — the allowlist that lets an
// authenticated caller through is TestAuthenticatedAllowedPaths's.
func TestServerInfoRouteServesTheName(t *testing.T) {
	status, body := getServerInfo(t, newServerInfoRouterForTest(services.ServerInfo{Name: "workstation"}))
	if status != http.StatusOK {
		t.Fatalf("GET /server status = %d, want %d", status, http.StatusOK)
	}
	if body["name"] != "workstation" {
		t.Fatalf("name = %v, want %q", body["name"], "workstation")
	}
}

// A server with no hostname and no name setting still answers, with the field
// absent, so a client can tell "unnamed" from "cannot ask" and name it after
// its address.
func TestServerInfoRouteOmitsAnEmptyName(t *testing.T) {
	status, body := getServerInfo(t, newServerInfoRouterForTest(services.ServerInfo{}))
	if status != http.StatusOK {
		t.Fatalf("GET /server status = %d, want %d", status, http.StatusOK)
	}
	if value, ok := body["name"]; ok {
		t.Fatalf("name = %v, want it absent on an unnamed server", value)
	}
}
