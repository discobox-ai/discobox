package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	poolapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/service"
	services "github.com/discobox-ai/discobox/server/internal/services"
	transport "github.com/discobox-ai/discobox/server/internal/transport"
)

// inventoryReadFixture is a router over one sandbox in the given runtime
// state, whose pool answers every request with poolAnswer.
func inventoryReadFixture(t *testing.T, runtimeState string, poolAnswer http.HandlerFunc) (http.Handler, *routerTestServices, *int) {
	t.Helper()
	stubs := newRouterTestServices()
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       testDefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		PoolID:          "pool-1",
		RuntimeState:    runtimeState,
	}
	stubs.inventory = []byte(`{"execs":[],"observedAt":"2026-10-11T00:00:00Z"}`)
	reached := new(int)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached++
		poolAnswer(w, r)
	}))
	t.Cleanup(upstream.Close)
	stubs.sandboxLease = transport.NewHTTPClientLeaseWithBaseURLAndAuth(upstream.Client(), upstream.URL, "worker-token", func() {})
	stubs.sandboxLease.ForwardAuthTokenProvider = func(context.Context) (string, error) { return "sandbox-agent-token", nil }
	router, err := NewRouter(services.Services{
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Pools:          stubs,
		Jobs:           stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	return router, stubs, reached
}

func liveAnswer(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"live":true}`))
}

// The pool agent's refusal of a read of a sandbox it found not running.
func notRunningAnswer(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(poolapimodel.SandboxNotRunningHeader, "stopped")
	http.Error(w, "discobox sandbox-1 is stopped: what is read here is inside it, and reading it does not start it", http.StatusConflict)
}

var inventoryReadRoutes = []struct {
	path string
	read services.InventoryRead
}{
	{"/execs", services.InventoryRead{Part: services.InventoryExecs}},
	{"/execs/exec-1", services.InventoryRead{Part: services.InventoryExec, ID: "exec-1"}},
	// An exec named like a route below it is still an exec.
	{"/execs/screen", services.InventoryRead{Part: services.InventoryExec, ID: "screen"}},
	{"/execs/exec-1/screen", services.InventoryRead{Part: services.InventoryScreen, ID: "exec-1"}},
	{"/services", services.InventoryRead{Part: services.InventoryServices}},
	{"/services/web", services.InventoryRead{Part: services.InventoryService, ID: "web"}},
	{"/tools", services.InventoryRead{Part: services.InventoryTools}},
}

func getSandboxRoute(router http.Handler, method, path string, scopes ...string) *httptest.ResponseRecorder {
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, scopedUserRequest(context.Background(), method, "/api/projects/"+testDefaultProjectID+"/sandboxes/sandbox-1"+path, nil, scopes...))
	return resp
}

// A sandbox its pool agent finds not running is read from its last inventory:
// the pool agent refuses rather than starts it, and marks the refusal
// (ADR 26-10-11-657 §4). The row's state decides nothing — it can be behind
// either way.
func TestASandboxThePoolFindsNotRunningIsReadFromItsInventory(t *testing.T) {
	for _, tc := range inventoryReadRoutes {
		t.Run(tc.path, func(t *testing.T) {
			router, stubs, reached := inventoryReadFixture(t, model.SandboxRuntimeStateStopped, notRunningAnswer)
			resp := getSandboxRoute(router, http.MethodGet, tc.path, poolagentauth.ScopeExecRead)
			if resp.Code != http.StatusOK || resp.Body.String() != string(stubs.inventory) {
				t.Fatalf("status = %d, body = %s; want the inventory's answer", resp.Code, resp.Body.String())
			}
			if got := resp.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Fatalf("Content-Type = %q", got)
			}
			if resp.Header().Get(poolapimodel.SandboxNotRunningHeader) != "" {
				t.Fatal("the pool's refusal header reached the client on an answered read")
			}
			if *reached != 1 {
				t.Fatalf("pool reached %d times, want once", *reached)
			}
			if !slices.Equal(stubs.inventoryReads, []services.InventoryRead{tc.read}) {
				t.Fatalf("inventory reads = %+v, want %+v", stubs.inventoryReads, tc.read)
			}
			if !slices.Equal(stubs.sandboxScopes, []string{poolagentauth.ScopeExecRead}) {
				t.Fatalf("the stored read was authorized with scopes %v", stubs.sandboxScopes)
			}
		})
	}
}

// A sandbox that an action has just started is running before its row says
// so. Its reads are its own answers, not the inventory's, which does not hold
// the exec the action created.
func TestARunningSandboxWhoseRowStillSaysStoppedIsReadLive(t *testing.T) {
	router, stubs, reached := inventoryReadFixture(t, model.SandboxRuntimeStateStopped, liveAnswer)
	resp := getSandboxRoute(router, http.MethodGet, "/execs/exec-1", poolagentauth.ScopeExecRead)
	if resp.Code != http.StatusOK || resp.Body.String() != `{"live":true}` {
		t.Fatalf("status = %d, body = %s; want the sandbox's own answer", resp.Code, resp.Body.String())
	}
	if *reached != 1 || len(stubs.inventoryReads) != 0 {
		t.Fatalf("pool reached %d times, inventory read %d times", *reached, len(stubs.inventoryReads))
	}
}

// A stopped sandbox whose pool cannot be asked is still read, from its
// inventory.
func TestAStoppedSandboxOnAnUnreachablePoolIsReadFromItsInventory(t *testing.T) {
	router, stubs, reached := inventoryReadFixture(t, model.SandboxRuntimeStateStopped, liveAnswer)
	stubs.poolUnreachable = true
	resp := getSandboxRoute(router, http.MethodGet, "/services", poolagentauth.ScopeExecRead)
	if resp.Code != http.StatusOK || resp.Body.String() != string(stubs.inventory) {
		t.Fatalf("status = %d, body = %s; want the inventory's answer", resp.Code, resp.Body.String())
	}
	if *reached != 0 || stubs.sandboxLeases != 0 {
		t.Fatalf("pool reached %d times, %d leases; want neither", *reached, stubs.sandboxLeases)
	}
}

// A running sandbox answers for itself.
func TestARunningSandboxIsReadLive(t *testing.T) {
	for _, tc := range inventoryReadRoutes {
		t.Run(tc.path, func(t *testing.T) {
			router, stubs, reached := inventoryReadFixture(t, model.SandboxRuntimeStateRunning, liveAnswer)
			resp := getSandboxRoute(router, http.MethodGet, tc.path, poolagentauth.ScopeExecRead)
			if resp.Code != http.StatusOK || resp.Body.String() != `{"live":true}` {
				t.Fatalf("status = %d, body = %s; want the sandbox's own answer", resp.Code, resp.Body.String())
			}
			if *reached != 1 || len(stubs.inventoryReads) != 0 {
				t.Fatalf("pool reached %d times, inventory read %d times", *reached, len(stubs.inventoryReads))
			}
		})
	}
}

// Only the marked refusal falls back. A 409 that means something else — the
// sandbox agent's own, for an exec with no terminal — is the answer.
func TestAConflictThatIsNotTheNotRunningRefusalIsRelayed(t *testing.T) {
	router, stubs, _ := inventoryReadFixture(t, model.SandboxRuntimeStateRunning, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"sandbox exec has no terminal"}`, http.StatusConflict)
	})
	resp := getSandboxRoute(router, http.MethodGet, "/execs/exec-1/screen", poolagentauth.ScopeExecRead)
	if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "no terminal") {
		t.Fatalf("status = %d, body = %s; want the sandbox agent's conflict relayed", resp.Code, resp.Body.String())
	}
	if len(stubs.inventoryReads) != 0 {
		t.Fatal("an unmarked conflict was answered from the inventory")
	}
}

// A sandbox that is not running and has posted nothing is refused, in the
// JSON shape these routes fail in.
func TestASandboxNotRunningWithNoInventoryIsRefused(t *testing.T) {
	router, stubs, _ := inventoryReadFixture(t, model.SandboxRuntimeStateStopped, notRunningAnswer)
	stubs.inventoryErr = apperrors.NewStatusError(http.StatusConflict, "discobox sandbox-1 is not running and has posted nothing to read")
	resp := getSandboxRoute(router, http.MethodGet, "/services", poolagentauth.ScopeExecRead)
	if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), `"error":"discobox sandbox-1 is not running and has posted nothing to read"`) {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
}

// Everything that is not one of those reads still reaches the pool of a
// stopped sandbox, which is what starts it: an action, and the reads with no
// last value to answer with (ADR 26-10-11-657 §5).
func TestActionsAndUnboundedReadsStillReachAStoppedSandbox(t *testing.T) {
	for _, tc := range []struct{ method, path, scope string }{
		{http.MethodPost, "/execs", poolagentauth.ScopeExecWrite},
		{http.MethodDelete, "/execs/exec-1", poolagentauth.ScopeExecWrite},
		{http.MethodPost, "/execs/exec-1/input", poolagentauth.ScopeExecWrite},
		{http.MethodPost, "/execs/exec-1/start", poolagentauth.ScopeExecWrite},
		{http.MethodPost, "/services/web/start", poolagentauth.ScopeExecWrite},
		{http.MethodGet, "/execs/exec-1/logs", poolagentauth.ScopeExecRead},
		{http.MethodGet, "/execs/exec-1/events", poolagentauth.ScopeExecRead},
		{http.MethodGet, "/execs/exec-1/resources", poolagentauth.ScopeExecRead},
		{http.MethodGet, "/execs/exec-1/resources/history", poolagentauth.ScopeExecRead},
		{http.MethodGet, "/services/web/logs", poolagentauth.ScopeExecRead},
		{http.MethodPost, "/execs/exec-1/wait", poolagentauth.ScopeExecRead},
		{http.MethodGet, "/exec-events", poolagentauth.ScopeExecRead},
		{http.MethodGet, "/harness-hooks", poolagentauth.ScopeExecRead},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			router, stubs, reached := inventoryReadFixture(t, model.SandboxRuntimeStateStopped, liveAnswer)
			resp := getSandboxRoute(router, tc.method, tc.path, tc.scope)
			if resp.Code != http.StatusOK || *reached != 1 {
				t.Fatalf("status = %d, pool reached %d times; want it proxied", resp.Code, *reached)
			}
			if len(stubs.inventoryReads) != 0 {
				t.Fatalf("answered from the inventory: %+v", stubs.inventoryReads)
			}
		})
	}
}

// An inventory report is refused past its bound before the generated server
// buffers it: a pool agent keeps its reports well under this, so a larger one
// is not a report.
func TestAnOversizedInventoryReportIsRefusedBeforeItIsBuffered(t *testing.T) {
	read := 0
	body := readerFunc(func(p []byte) (int, error) {
		for i := range p {
			p[i] = ' '
		}
		read += len(p)
		return len(p), nil
	})
	reached := false
	handler := boundInventoryReports(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if _, err := io.ReadAll(r.Body); err == nil {
			t.Error("an endless body was read whole")
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}))
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/pool-agent/pool-1/sandbox-inventories", body))
	if !reached || read > maxInventoryReportBodyBytes+(1<<20) {
		t.Fatalf("reached = %v, read %d bytes; want the body cut off near %d", reached, read, maxInventoryReportBodyBytes)
	}

	// Every other route's body is left alone.
	other := boundInventoryReports(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if data, err := io.ReadAll(r.Body); err != nil || len(data) != 5 {
			t.Errorf("another route's body: %q, %v", data, err)
		}
	}))
	other.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/pool-agent/pool-1/sandbox-agent-status", strings.NewReader("hello")))
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
