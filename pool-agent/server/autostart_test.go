package server

import (
	"context"
	"errors"
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

func TestAutoStartPreservesStartupFailure(t *testing.T) {
	bindMount := errors.New("invalid mount config: bind source path does not exist")
	for _, tc := range []struct {
		name    string
		need    sandboxNeed
		err     error
		status  int
		proxied bool
	}{
		{"missing bind mount", needsSandbox, bindMount, http.StatusInternalServerError, false},
		{"unknown sandbox", needsSandbox, sandboxruntime.ErrNotFound, http.StatusNotFound, false},
		{"archived sandbox", needsSandbox, sandboxruntime.ErrArchived, http.StatusConflict, false},
		{"missing container", needsSandbox, sandboxruntime.ErrNoContainer, http.StatusConflict, false},
		{"running sandbox", needsSandbox, nil, http.StatusNoContent, true},
		// The pool serves origins from its own files, so a source's origin can
		// be fetched while its sandbox cannot start.
		{"an origin of a sandbox that cannot start", servedByPool, bindMount, http.StatusNoContent, true},
		{"an origin of an archived sandbox", servedByPool, sandboxruntime.ErrArchived, http.StatusConflict, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &sandboxService{runtime: &ensureRecorder{err: tc.err}}
			proxied := false
			router := chi.NewRouter()
			router.Get("/sandboxes/{sandboxId}/attach", service.autoStart(awaitContainer, tc.need, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				proxied = true
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/sandboxes/sbx_1/attach", nil))
			if rec.Code != tc.status || proxied != tc.proxied {
				t.Fatalf("status %d, proxied %v, body %q", rec.Code, proxied, rec.Body.String())
			}
			if !tc.proxied && !strings.Contains(rec.Body.String(), tc.err.Error()) {
				t.Fatalf("startup failure lost: %q", rec.Body.String())
			}
		})
	}
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

// bootingRuntime is a runtime holding one sandbox whose container Docker calls
// running, and whose boot may still be under way.
type bootingRuntime struct {
	sandboxruntime.Runtime
	booting bool
}

func (r *bootingRuntime) GetSandbox(context.Context, string) (*sandboxruntime.Sandbox, error) {
	return &sandboxruntime.Sandbox{SandboxID: "sbx_1", Status: sandboxruntime.StatusRunning}, nil
}

func (r *bootingRuntime) SandboxBooting(string) bool { return r.booting }

// A read of what is inside a sandbox is refused, not proxied, while the
// sandbox is still booting: its agent is not answering yet, so passing the read
// through would come back a bare 502 instead of saying why.
func TestRequireRunningRefusesASandboxStillBooting(t *testing.T) {
	served := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true })
	for name, tc := range map[string]struct {
		booting    bool
		wantServed bool
	}{
		"booting": {booting: true},
		"running": {booting: false, wantServed: true},
	} {
		t.Run(name, func(t *testing.T) {
			served = false
			service := &sandboxService{runtime: &bootingRuntime{booting: tc.booting}}
			router := chi.NewRouter()
			router.Handle("/sandboxes/{sandboxId}/screen", service.requireRunning(next))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/sandboxes/sbx_1/screen", nil))
			if served != tc.wantServed {
				t.Fatalf("served = %v, want %v (status %d, body %q)", served, tc.wantServed, rec.Code, rec.Body.String())
			}
			if !tc.wantServed && (rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "is starting")) {
				t.Fatalf("status %d body %q, want 409 saying it is starting", rec.Code, rec.Body.String())
			}
		})
	}
}
