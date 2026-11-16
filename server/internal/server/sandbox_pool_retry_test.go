package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/service"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

// shortPoolOutage makes the attach's outage window and its tries short, for
// the test.
func shortPoolOutage(t *testing.T, window time.Duration) {
	t.Helper()
	oldWindow, oldInterval := sandboxPoolOutageWindow, sandboxPoolOutageRetryInterval
	sandboxPoolOutageWindow, sandboxPoolOutageRetryInterval = window, 10*time.Millisecond
	t.Cleanup(func() { sandboxPoolOutageWindow, sandboxPoolOutageRetryInterval = oldWindow, oldInterval })
}

// poolOutageRouter is a router whose sandbox's pool is upstream.
func poolOutageRouter(t *testing.T, upstream http.Handler, leases ...func(*transport.HTTPClientLease)) http.Handler {
	t.Helper()
	stubs := newRouterTestServices()
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID: "sandbox-1", ProjectID: testDefaultProjectID,
		CreatedByUserID: service.DefaultUserID, PoolID: "pool-1",
	}
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	stubs.sandboxLease = transport.NewHTTPClientLeaseWithBaseURLAndAuth(server.Client(), server.URL, "worker-token", nil)
	for _, lease := range leases {
		lease(stubs.sandboxLease)
	}
	router, err := NewRouter(services.Services{
		Projects: stubs, HarnessConfigs: stubs, Sandboxes: stubs,
		Providers: stubs, Pools: stubs, Jobs: stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	return router
}

var poolOutageBase = "/api/projects/" + testDefaultProjectID + "/sandboxes/sandbox-1"

func serveAttach(router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, scopedUserRequest(context.Background(), method, poolOutageBase+path, reader, poolagentauth.ScopeExecWrite, poolagentauth.ScopeExecRead))
	return resp
}

// An attach made while a hop to the pool is down — a front answering that
// nothing is behind it, or a connection cut with no answer — reaches the pool
// once it is back, authenticated each time, rather than failing at once.
func TestAnAttachWaitsOutABriefPoolOutage(t *testing.T) {
	shortPoolOutage(t, 5*time.Second)
	var tries atomic.Int32
	router := poolOutageRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer worker-token" {
			t.Errorf("try %d: upstream auth = %q", tries.Load()+1, got)
		}
		switch tries.Add(1) {
		case 1:
			http.Error(w, "no healthy upstream", http.StatusBadGateway)
		case 2:
			// Cut with no answer, as an edge restarting does.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
		case 3:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	resp := serveAttach(router, http.MethodGet, "/execs/e1/attach", "")
	if resp.Code != http.StatusNoContent {
		t.Fatalf("attach: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if got := tries.Load(); got != 4 {
		t.Fatalf("upstream tries = %d, want 4", got)
	}
}

// A pool that stays away past the window is answered as before: the last
// front's answer, or the refusal for one that never answered.
func TestAnAttachStopsWaitingForAPoolAfterTheWindow(t *testing.T) {
	shortPoolOutage(t, 200*time.Millisecond)
	var tries atomic.Int32
	router := poolOutageRouter(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tries.Add(1)
		http.Error(w, "no healthy upstream", http.StatusBadGateway)
	}))
	started := time.Now()
	resp := serveAttach(router, http.MethodGet, "/execs/e1/attach", "")
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("attach: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if tries.Load() < 2 {
		t.Fatalf("upstream tries = %d, want the attach retried", tries.Load())
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("attach took %s, want it bounded by the window", elapsed)
	}
}

// Only an attach waits: every other request into the sandbox answers now,
// and an attach whose body is the client's stream goes once.
func TestOnlyABodilessAttachWaitsForThePool(t *testing.T) {
	shortPoolOutage(t, 5*time.Second)
	var tries atomic.Int32
	router := poolOutageRouter(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tries.Add(1)
		http.Error(w, "no healthy upstream", http.StatusBadGateway)
	}))
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/execs/e1/logs", ""},
		{http.MethodPost, "/execs/e1/start", ""},
		{http.MethodPost, "/execs/e1/attach", "stdin"},
	} {
		tries.Store(0)
		resp := serveAttach(router, tc.method, tc.path, tc.body)
		if resp.Code != http.StatusBadGateway {
			t.Errorf("%s %s: status = %d, want the 502 passed on", tc.method, tc.path, resp.Code)
		}
		if got := tries.Load(); got != 1 {
			t.Errorf("%s %s: upstream tries = %d, want 1", tc.method, tc.path, got)
		}
	}
}

// The pool's own answer is no outage, even a 502: the pool agent that cannot
// reach the sandbox, or the sandbox agent's failed attach, answer in the
// API's JSON error shape, and that is handed on at once. So is a failure that
// is no connection's: a token that could not be minted.
func TestAnAttachHandsOnThePoolsOwnRefusalAtOnce(t *testing.T) {
	shortPoolOutage(t, 5*time.Second)
	var tries atomic.Int32
	router := poolOutageRouter(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tries.Add(1)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":"the discobox could not be reached: it may be restarting"}`)
	}))
	resp := serveAttach(router, http.MethodGet, "/execs/e1/attach", "")
	if resp.Code != http.StatusBadGateway || !strings.Contains(resp.Body.String(), "may be restarting") {
		t.Fatalf("attach: status = %d, body = %s, want the pool's own 502", resp.Code, resp.Body.String())
	}
	if got := tries.Load(); got != 1 {
		t.Fatalf("upstream tries = %d, want 1", got)
	}

	tries.Store(0)
	var mints atomic.Int32
	router = poolOutageRouter(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tries.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}), func(lease *transport.HTTPClientLease) {
		lease.AuthTokenProvider = func(context.Context) (string, error) {
			mints.Add(1)
			return "", errors.New("the pool's signing key is not available")
		}
	})
	resp = serveAttach(router, http.MethodGet, "/execs/e1/attach", "")
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("attach with no token: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if got := mints.Load(); got != 1 {
		t.Fatalf("token mints = %d, want 1: a failure that is no connection's is not retried", got)
	}
	if got := tries.Load(); got != 0 {
		t.Fatalf("upstream tries = %d, want 0", got)
	}
}
