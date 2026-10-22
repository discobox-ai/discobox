package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// The files API is forwarded on its own scopes (ADR 26-10-22-499 §3), told by
// the whole route: an exec or a service named "files" is still what it is.
func TestFilesAreForwardedOnTheirOwnScopes(t *testing.T) {
	const base = "/api/project/p/pool/w/sandboxes/sb_1"
	for _, tc := range []struct {
		method, path, want string
	}{
		{http.MethodGet, "/files/stat", ScopeFilesRead},
		{http.MethodGet, "/files/list", ScopeFilesRead},
		{http.MethodGet, "/files/content", ScopeFilesRead},
		{http.MethodPut, "/files/content", ScopeFilesWrite},
		{http.MethodPost, "/files/mkdir", ScopeFilesWrite},
		{http.MethodPost, "/files/remove", ScopeFilesWrite},
		{http.MethodPost, "/files/move", ScopeFilesWrite},
		{http.MethodPost, "/files/copy", ScopeFilesWrite},
		{http.MethodGet, "/execs/files", ScopeExecRead},
		{http.MethodGet, "/execs/files/logs", ScopeExecRead},
		{http.MethodPost, "/services/files/start", ScopeExecWrite},
	} {
		req := httptest.NewRequestWithContext(t.Context(), tc.method, base+tc.path, nil)
		if got := sandboxAgentRequiredScope(req); got != tc.want {
			t.Errorf("%s %s: scope = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

// Each files route is registered by name, as this router forwards only the
// paths it names, and starts a stopped sandbox by the use it is marked with.
func TestFilesRoutesAreRegisteredAndStartTheSandbox(t *testing.T) {
	runtime := &ensureRecorder{err: sandboxruntime.ErrNoContainer}
	router := chi.NewRouter()
	registerSandboxProxyRoutes(router, &sandboxService{runtime: runtime})
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/files/stat"},
		{http.MethodGet, "/files/list"},
		{http.MethodGet, "/files/content"},
		{http.MethodPut, "/files/content"},
		{http.MethodPost, "/files/mkdir"},
		{http.MethodPost, "/files/remove"},
		{http.MethodPost, "/files/move"},
		{http.MethodPost, "/files/copy"},
	} {
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, markedRequest(t, route.method, "/api/project/p/pool/w/sandboxes/sb_1"+route.path, UseRead))
		if resp.Code == http.StatusNotFound || resp.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status = %d, the route is not registered", route.method, route.path, resp.Code)
		}
	}
	if runtime.ensured != 8 {
		t.Fatalf("ensured %d times, want each route to start the sandbox", runtime.ensured)
	}
}
