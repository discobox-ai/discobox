package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// ensureRecorder is a runtime whose only behavior is EnsureSandboxRunning: it
// records whether the route asked to wait, and fails the way a sandbox with no
// container does.
type ensureRecorder struct {
	sandboxruntime.Runtime
	awaited []bool
	err     error
}

func (r *ensureRecorder) EnsureSandboxRunning(_ context.Context, _ string, awaitContainer bool) error {
	r.awaited = append(r.awaited, awaitContainer)
	return r.err
}

// Only exec attach waits for a container to be rebuilt, because it is the only
// route the control plane waits on; everything else is answered at once, so a
// failed sandbox whose container is gone does not hold `discobox tools`,
// service listings, tunnels or git for the whole rebuild wait.
func TestAutoStartWaitsForAContainerOnlyWhereTheControlPlaneDoes(t *testing.T) {
	runtime := &ensureRecorder{err: sandboxruntime.ErrNoContainer}
	service := &sandboxService{runtime: runtime}
	router := chi.NewRouter()
	registerSandboxProxyRoutes(router, service)
	registerSandboxGitRoutes(router, service)

	base := "/api/project/p/pool/w/sandboxes/sbx_1"
	for path, wantWait := range map[string]bool{
		base + "/tools":                   false,
		base + "/services":                false,
		base + "/execs":                   false,
		base + "/execs/ex_1/attach":       true,
		base + "/tcp/attach":              false,
		base + "/http/8080/":              false,
		base + "/git-repositories/r/info": false,
	} {
		runtime.awaited = nil
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if len(runtime.awaited) != 1 || runtime.awaited[0] != wantWait {
			t.Errorf("%s: awaited = %v, want %v", path, runtime.awaited, wantWait)
		}
		// A sandbox with no container is a conflict the caller can act on,
		// not a missing sandbox.
		if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "needs repair") {
			t.Errorf("%s: %d %q, want 409 naming repair", path, recorder.Code, recorder.Body.String())
		}
	}
}
