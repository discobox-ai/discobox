package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// ensureRecorder is a runtime whose only behavior is EnsureSandboxRunning: it
// counts the calls, and fails the way a sandbox with no container does.
type ensureRecorder struct {
	sandboxruntime.Runtime
	ensured int
	err     error
}

func (r *ensureRecorder) EnsureSandboxRunning(context.Context, string) error {
	r.ensured++
	return r.err
}

// markedRequest is a request whose token the control plane marked with use.
func markedRequest(t *testing.T, method, path, use string) *http.Request {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	return request.WithContext(withSignedTokenClaims(request.Context(), SignedTokenClaims{Use: use}))
}

func TestStartOnUsePreservesStartupFailure(t *testing.T) {
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
			router.Get("/sandboxes/{sandboxId}/attach", service.onUse(tc.need, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				proxied = true
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, markedRequest(t, http.MethodGet, "/sandboxes/sbx_1/attach", UseAction))
			if rec.Code != tc.status || proxied != tc.proxied {
				t.Fatalf("status %d, proxied %v, body %q", rec.Code, proxied, rec.Body.String())
			}
			if !tc.proxied && !strings.Contains(rec.Body.String(), tc.err.Error()) {
				t.Fatalf("startup failure lost: %q", rec.Body.String())
			}
		})
	}
}

// No route waits for a container to be rebuilt: the control plane is the tier
// that knows whether one is coming, and waits there (ADR 26-10-12-711 §6). A
// failed sandbox whose container is gone answers every route at once, so it
// does not hold an attach, `discobox tools`, service listings, tunnels or git.
func TestNoRouteWaitsForAContainer(t *testing.T) {
	runtime := &ensureRecorder{err: sandboxruntime.ErrNoContainer}
	service := &sandboxService{runtime: runtime}
	router := chi.NewRouter()
	registerSandboxProxyRoutes(router, service)
	registerSandboxGitRoutes(router, service)

	base := "/api/project/p/pool/w/sandboxes/sbx_1"
	for _, path := range []string{
		base + "/execs/ex_1/logs",
		base + "/services/web/logs",
		base + "/execs/ex_1/events",
		base + "/execs/ex_1/attach",
		base + "/tcp/attach",
		base + "/http/8080/",
		base + "/git-repositories/r/info",
	} {
		runtime.ensured = 0
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, markedRequest(t, http.MethodGet, path, UseAction))
		if runtime.ensured != 1 {
			t.Errorf("%s: ensured %d times, want once", path, runtime.ensured)
		}
		// A sandbox with no container is a conflict the caller can act on,
		// not a missing sandbox.
		if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "needs repair") {
			t.Errorf("%s: %d %q, want 409 naming repair", path, recorder.Code, recorder.Body.String())
		}
	}
}

// useRuntime is a runtime holding one stopped sandbox, which counts the starts
// it is asked for.
type useRuntime struct {
	sandboxruntime.Runtime
	ensured int
}

func (r *useRuntime) EnsureSandboxRunning(context.Context, string) error {
	r.ensured++
	return nil
}

func (r *useRuntime) GetSandbox(context.Context, string) (*sandboxruntime.Sandbox, error) {
	return &sandboxruntime.Sandbox{SandboxID: "sbx_1", Status: sandboxruntime.StatusStopped}, nil
}

func (r *useRuntime) SandboxBooting(string) bool { return false }

// Whether a request starts a stopped sandbox is the control plane's decision,
// carried on the token (ADR 26-10-12-711 §2), and the same route obeys
// whichever it is given: an action or a read starts the sandbox; an inspect
// and an observe are refused without starting it, marked so the control plane
// can answer from the sandbox's last inventory; and so is a token with no mark.
func TestOnUseObeysTheTokensUse(t *testing.T) {
	for _, tc := range []struct {
		use        string
		wantStart  bool
		wantStatus int
	}{
		{UseAction, true, http.StatusNoContent},
		{UseRead, true, http.StatusNoContent},
		{UseInspect, false, http.StatusConflict},
		{UseObserve, false, http.StatusConflict},
		{"", false, http.StatusConflict},
	} {
		t.Run("use="+tc.use, func(t *testing.T) {
			runtime := &useRuntime{}
			service := &sandboxService{runtime: runtime}
			router := chi.NewRouter()
			router.Handle("/sandboxes/{sandboxId}/execs", service.onUse(needsSandbox, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, markedRequest(t, http.MethodGet, "/sandboxes/sbx_1/execs", tc.use))
			if started := runtime.ensured == 1; started != tc.wantStart || rec.Code != tc.wantStatus {
				t.Fatalf("started = %v, status %d (%q); want started = %v, status %d", started, rec.Code, rec.Body.String(), tc.wantStart, tc.wantStatus)
			}
			if !tc.wantStart && !strings.Contains(rec.Body.String(), "does not start it") {
				t.Fatalf("an observe of a stopped sandbox answered %q, want it to say reading does not start it", rec.Body.String())
			}
			if !tc.wantStart && rec.Header().Get(workerapimodel.SandboxNotRunningHeader) != "stopped" {
				t.Fatalf("%s = %q, want the refusal marked stopped", workerapimodel.SandboxNotRunningHeader, rec.Header().Get(workerapimodel.SandboxNotRunningHeader))
			}
		})
	}
}

// bootingRuntime is a runtime holding one sandbox whose container Docker calls
// running, and whose boot may still be under way. Waiting the boot out ends
// it, leaving the sandbox in afterBoot.
type bootingRuntime struct {
	sandboxruntime.Runtime
	booting   bool
	afterBoot sandboxruntime.Status
	awaited   int
	status    sandboxruntime.Status
}

func (r *bootingRuntime) GetSandbox(context.Context, string) (*sandboxruntime.Sandbox, error) {
	status := r.status
	if status == "" {
		status = sandboxruntime.StatusRunning
	}
	return &sandboxruntime.Sandbox{SandboxID: "sbx_1", Status: status}, nil
}

func (r *bootingRuntime) SandboxBooting(string) bool { return r.booting }

func (r *bootingRuntime) AwaitSandboxBoot(context.Context, string) error {
	r.awaited++
	r.booting = false
	r.status = r.afterBoot
	return nil
}

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
			runtime := &bootingRuntime{booting: tc.booting}
			service := &sandboxService{runtime: runtime}
			router := chi.NewRouter()
			router.Handle("/sandboxes/{sandboxId}/screen", service.requireRunning(refuseBooting, next))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/sandboxes/sbx_1/screen", nil))
			if served != tc.wantServed {
				t.Fatalf("served = %v, want %v (status %d, body %q)", served, tc.wantServed, rec.Code, rec.Body.String())
			}
			if !tc.wantServed && (rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "is starting")) {
				t.Fatalf("status %d body %q, want 409 saying it is starting", rec.Code, rec.Body.String())
			}
			if !tc.wantServed && rec.Header().Get(workerapimodel.SandboxNotRunningHeader) != "starting" {
				t.Fatalf("%s = %q, want starting", workerapimodel.SandboxNotRunningHeader, rec.Header().Get(workerapimodel.SandboxNotRunningHeader))
			}
			if runtime.awaited != 0 {
				t.Fatal("a route that refuses a booting sandbox waited its boot out")
			}
		})
	}
}

// The lists wait out a boot something else began, and start nothing: a client
// that has just created or started a discobox lists it next, and must get the
// discobox's own answer rather than what it posted before it last stopped. A
// boot can end without the sandbox up, and then the read is refused as for
// any sandbox that is not running.
func TestRequireRunningWaitsOutABootForTheLists(t *testing.T) {
	for name, tc := range map[string]struct {
		before     sandboxruntime.Status
		afterBoot  sandboxruntime.Status
		wantServed bool
	}{
		"the boot ends with the sandbox up": {afterBoot: sandboxruntime.StatusRunning, wantServed: true},
		// A boot is marked before its container is started.
		"the container has not started yet":   {before: sandboxruntime.StatusStopped, afterBoot: sandboxruntime.StatusRunning, wantServed: true},
		"the boot ends with the sandbox down": {afterBoot: sandboxruntime.StatusStopped},
	} {
		t.Run(name, func(t *testing.T) {
			served := false
			runtime := &bootingRuntime{booting: true, status: tc.before, afterBoot: tc.afterBoot}
			service := &sandboxService{runtime: runtime}
			router := chi.NewRouter()
			router.Handle("/sandboxes/{sandboxId}/execs", service.requireRunning(waitOutBoot, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true })))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/sandboxes/sbx_1/execs", nil))
			if runtime.awaited != 1 {
				t.Fatalf("boot awaited %d times, want once", runtime.awaited)
			}
			if served != tc.wantServed {
				t.Fatalf("served = %v, want %v (status %d, body %q)", served, tc.wantServed, rec.Code, rec.Body.String())
			}
			if !tc.wantServed && (rec.Code != http.StatusConflict || rec.Header().Get(workerapimodel.SandboxNotRunningHeader) != "stopped") {
				t.Fatalf("status %d, %s = %q; want 409 marked stopped", rec.Code, workerapimodel.SandboxNotRunningHeader, rec.Header().Get(workerapimodel.SandboxNotRunningHeader))
			}
		})
	}
}

// An inspect waits out a boot something else began and an observe does not:
// that difference is the token's too, not the route's.
func TestOnUseWaitsOutABootOnlyForAnInspect(t *testing.T) {
	for use, wantAwaited := range map[string]int{UseInspect: 1, UseObserve: 0, "": 0} {
		t.Run("use="+use, func(t *testing.T) {
			runtime := &bootingRuntime{booting: true, afterBoot: sandboxruntime.StatusRunning}
			service := &sandboxService{runtime: runtime}
			served := false
			router := chi.NewRouter()
			router.Handle("/sandboxes/{sandboxId}/execs", service.onUse(needsSandbox, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true })))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, markedRequest(t, http.MethodGet, "/sandboxes/sbx_1/execs", use))
			if runtime.awaited != wantAwaited || served != (wantAwaited == 1) {
				t.Fatalf("boot awaited %d times, served = %v (status %d); want %d and served only after a wait", runtime.awaited, served, rec.Code, wantAwaited)
			}
		})
	}
}

// A sandbox whose machine its provider paused is reported as the pause it
// is: a read of what is inside it is refused as suspended, and the pool API
// reports it suspended, as the state channel does.
func TestAPausedSandboxIsSuspendedToAReadAndToTheAPI(t *testing.T) {
	served := false
	runtime := &bootingRuntime{status: sandboxruntime.StatusPaused}
	service := &sandboxService{runtime: runtime}
	router := chi.NewRouter()
	router.Handle("/sandboxes/{sandboxId}/execs", service.requireRunning(refuseBooting, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true })))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/sandboxes/sbx_1/execs", nil))
	if served || rec.Code != http.StatusConflict || rec.Header().Get(workerapimodel.SandboxNotRunningHeader) != "suspended" {
		t.Fatalf("served %v, status %d, %s = %q; want 409 marked suspended", served, rec.Code, workerapimodel.SandboxNotRunningHeader, rec.Header().Get(workerapimodel.SandboxNotRunningHeader))
	}
	out := sandboxInstanceFromRuntime(&sandboxruntime.Sandbox{SandboxID: "sbx_1", Status: sandboxruntime.StatusPaused}, nil)
	if out.Runtime.Status != string(sandboxruntime.StatusSuspended) {
		t.Fatalf("a paused sandbox's runtime status = %q, want suspended", out.Runtime.Status)
	}
}
