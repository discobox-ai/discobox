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
// and the ordinary sandbox scopes do not reach it (ADR 26-09-22-838 §2).
func TestJudgeIsForwardedOnItsOwnScope(t *testing.T) {
	const path = "/api/project/project-1/pool/pool-1/sandboxes/sb_judge/judge"

	for _, tc := range []struct {
		name, method, path, want string
	}{
		{"putting a job to the judge", http.MethodPost, path, ScopeJudgeRun},
		// Proposing a rule's wording is the same runtime's other job.
		{"asking it to propose a rule", http.MethodPost, path + "/proposal", ScopeJudgeRun},
		{"reading a proposal", http.MethodGet, path + "/proposal", ""},
		// So is answering the questions about a request under a policy.
		{"asking it a policy's questions", http.MethodPost, path + "/policy", ScopeJudgeRun},
		{"reading a policy's answers", http.MethodGet, path + "/policy", ""},
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
// plane cannot tell from a pool that has no judge. It starts a stopped judge,
// and answers at once for one with no container.
func TestJudgeRouteIsRegisteredAndStartsAStoppedJudge(t *testing.T) {
	runtime := &ensureRecorder{err: sandboxruntime.ErrNoContainer}
	service := &sandboxService{runtime: runtime}
	router := chi.NewRouter()
	registerSandboxProxyRoutes(router, service)

	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, markedRequest(t, http.MethodPost, "/api/project/p/pool/w/sandboxes/sb_judge/judge", UseAction))
	if resp.Code == http.StatusNotFound {
		t.Fatal("status = 404: the judge's path is not registered with this router")
	}
	if runtime.ensured != 1 {
		t.Fatalf("ensured %d times, want the judge started once", runtime.ensured)
	}
}

// The judge runtime's other routes are registered by name too.
func TestTheJudgesOtherRoutesAreRegistered(t *testing.T) {
	for _, route := range []string{"/judge/policy", "/judge/proposal"} {
		runtime := &ensureRecorder{err: sandboxruntime.ErrNoContainer}
		service := &sandboxService{runtime: runtime}
		router := chi.NewRouter()
		registerSandboxProxyRoutes(router, service)

		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, markedRequest(t, http.MethodPost, "/api/project/p/pool/w/sandboxes/sb_judge"+route, UseAction))
		if resp.Code == http.StatusNotFound || resp.Code == http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want the path registered with this router", route, resp.Code)
		}
	}
}
