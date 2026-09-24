package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// Judging is its own authority on the way through the pool as well: a token
// that may ask the judge may read and write nothing in the sandbox it asks,
// and the ordinary sandbox scopes do not reach it (ADR 0149 §2).
func TestJudgeIsForwardedOnItsOwnScope(t *testing.T) {
	const path = "/api/project/project-1/pool/pool-1/sandboxes/sb_judge/judge"

	for _, tc := range []struct {
		name, method, path, want string
	}{
		{"putting a job to the judge", http.MethodPost, path, ScopeJudgeRun},
		// No scope is not a refusal — it means this hop checks none — so what
		// keeps a GET out is that the route is registered for POST alone.
		// The table is asked anyway, to keep it in step with the sandbox
		// agent's, which answers the same way.
		{"reading it", http.MethodGet, path, ""},
		// An exec that happens to be called "judge" is an exec. The two
		// tables order these the same way, so one hop cannot check a scope
		// the other does not.
		{"an exec called judge", http.MethodGet, "/api/project/p/pool/w/sandboxes/sb_1/execs/judge", ScopeExecRead},
		{"an exec beside it", http.MethodPost, "/api/project/project-1/pool/pool-1/sandboxes/sb_judge/execs", ScopeExecWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil)
			if got := sandboxAgentRequiredScope(req); got != tc.want {
				t.Fatalf("required scope = %q, want %q", got, tc.want)
			}
		})
	}
}

// The judge's route is registered by name, because this router forwards the
// paths it names and nothing else: an unregistered one is a 404 the control
// plane cannot tell from a pool that has no judge. It starts a stopped judge
// without waiting for a container to be rebuilt — the request a verdict is
// holding open would be waiting on that rebuild.
func TestJudgeRouteIsRegisteredAndStartsAStoppedJudge(t *testing.T) {
	runtime := &ensureRecorder{err: sandboxruntime.ErrNoContainer}
	service := &sandboxService{runtime: runtime}
	router := chi.NewRouter()
	registerSandboxProxyRoutes(router, service)

	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/project/p/pool/w/sandboxes/sb_judge/judge", nil))
	if resp.Code == http.StatusNotFound {
		t.Fatal("status = 404: the judge's path is not registered with this router")
	}
	if len(runtime.awaited) != 1 || runtime.awaited[0] {
		t.Fatalf("awaited = %v, want the judge started without waiting for a rebuild", runtime.awaited)
	}
}
