package poolruntime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/discobox-ai/discobox/pool-agent/createprotocol"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

func TestCreateNeverSendsBodyToLegacyAgentWithStaleReadyFlags(t *testing.T) {
	current := newTestRuntimeProvider(t, "project-1", "pool-1")
	var legacyCreates, probes atomic.Int32
	next := current.baseURL
	// A proxy's upstream changes after the legacy discovery. Its stale pool row
	// remains schedulable, but the legacy decoder must receive zero POSTs.
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if probes.Add(1) == 1 {
			if r.Method == http.MethodPost {
				legacyCreates.Add(1)
			}
			http.NotFound(w, r)
			return
		}
		// Dispatch subsequent discovery directly to a compatible instance through
		// an HTTP reverse proxy, including its WebSocket stream.
		http.Redirect(w, r, next+createprotocol.Path("project-1", "pool-1"), http.StatusTemporaryRedirect)
	}))
	defer agent.Close()
	current.baseURL = agent.URL
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(current, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, _, err := provider.Create(ctx, sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{PoolID: "pool-1"})
	if err != nil {
		t.Fatal(err)
	}
	if legacyCreates.Load() != 0 || manager.scheduledReconciles != 1 {
		t.Fatalf("legacy creates=%d reconciles=%d", legacyCreates.Load(), manager.scheduledReconciles)
	}
}

func TestCreateDiscoveryFailuresNeverSendCreate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		revision string
		retry    bool
	}{
		{"legacy", 404, "", true}, {"not ready", 503, "", true}, {"unauthenticated", 401, "", false}, {"unauthorized", 403, "", false},
		{"future revision", 101, fmt.Sprintf(`{"createRevision":%d}`, createprotocol.Revision+1), true}, {"malformed", 101, `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var creates atomic.Int32
			agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					creates.Add(1)
				}
				if tc.status != 101 {
					w.WriteHeader(tc.status)
					return
				}
				ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{createprotocol.Subprotocol}})
				if err != nil {
					return
				}
				defer ws.CloseNow()
				_ = ws.Write(r.Context(), websocket.MessageText, []byte(tc.revision))
				_, _, _ = ws.Read(r.Context())
			}))
			defer agent.Close()
			runtime := &testRuntimeProvider{baseURL: agent.URL, client: agent.Client(), token: "test"}
			manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
			provider := New(runtime, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer cancel()
			_, _, err := provider.Create(ctx, sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{PoolID: "pool-1"})
			if err == nil || creates.Load() != 0 {
				t.Fatalf("err=%v creates=%d", err, creates.Load())
			}
			if (manager.scheduledReconciles > 0) != tc.retry {
				t.Fatalf("scheduled=%d err=%v", manager.scheduledReconciles, err)
			}
			if !tc.retry && errors.Is(err, sandbox.ErrPoolAgentUnsupported) {
				t.Fatalf("fatal failure became compatibility wait: %v", err)
			}
		})
	}
}

func TestFirstDiscoveryFitsPoolCeiling(t *testing.T) {
	old := poolAgentWaitCeiling
	poolAgentWaitCeiling = 30 * time.Millisecond
	t.Cleanup(func() { poolAgentWaitCeiling = old })
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{createprotocol.Subprotocol}})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		_, _, _ = ws.Read(r.Context())
	}))
	defer agent.Close()
	runtime := &testRuntimeProvider{baseURL: agent.URL, client: agent.Client(), token: "token"}
	provider := New(runtime, sandbox.ProviderDefinition{Name: "test"}, &fakePoolManager{pool: activePool("pool-1"), schedulable: true}, nil)
	started := time.Now()
	_, _, err := provider.Create(t.Context(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{PoolID: "pool-1"})
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("first probe err=%v duration=%v", err, time.Since(started))
	}
	if strings.Contains(err.Error(), "no runtime") {
		t.Fatalf("discovery failure misclassified: %v", err)
	}
}

func TestDiscoveryTokenStaysScopedToItsSandbox(t *testing.T) {
	agent := newCreateProtocolTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer agent.Close()
	runtime := &testRuntimeProvider{baseURL: agent.URL, client: agent.Client()}
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtime, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
	err := provider.CheckCreateCompatibility(t.Context(), sandbox.SandboxRef{ProjectID: "project-1", PoolID: "pool-1", SandboxID: "only-this-box"})
	if err != nil {
		t.Fatal(err)
	}
	if len(manager.agentTokenClaims) != 1 || manager.agentTokenClaims[0].SandboxID != "only-this-box" {
		t.Fatalf("claims=%+v", manager.agentTokenClaims)
	}
}

func TestCreateAlreadyExistsReadsBackOnBoundStream(t *testing.T) {
	runtime := newTestRuntimeProvider(t, "project-1", "pool-1")
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtime, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
	ref := sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}
	if _, _, err := provider.Create(t.Context(), ref, nil, sandbox.CreateOptions{PoolID: "pool-1"}); err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse(runtime.baseURL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	reads := 0
	conflict := newCreateProtocolTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"title":"Conflict","status":409}`))
			return
		}
		reads++
		proxy.ServeHTTP(w, r)
	}))
	defer conflict.Close()
	runtime.baseURL = conflict.URL
	created, _, err := provider.Create(t.Context(), ref, nil, sandbox.CreateOptions{PoolID: "pool-1"})
	if err != nil || created == nil || created.SandboxID != "sandbox-1" || reads != 1 {
		t.Fatalf("create=%+v err=%v reads=%d", created, err, reads)
	}
}

// VM transports normalize native POSIX failures before HTTP wraps them. A
// discovery connection reset must wait for recovery rather than settle create.
func TestCreateDiscoveryWaitsForVSOCKRecovery(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ECONNRESET, syscall.ECONNREFUSED} {
		t.Run(errno.Error(), func(t *testing.T) {
			runtime := newTestRuntimeProvider(t, "project-1", "pool-1")
			var attempts atomic.Int32
			transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				if attempts.Add(1) == 1 {
					return nil, fmt.Errorf("vzvm: connect to pool-1 guest port 3002: %w", &net.OpError{Op: "dial", Net: "vsock", Err: os.NewSyscallError("connect", errno)})
				}
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}}
			t.Cleanup(transport.CloseIdleConnections)
			runtime.client = &http.Client{Transport: transport}
			manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
			provider := New(runtime, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			created, _, err := provider.Create(ctx, sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{PoolID: "pool-1"})
			if err != nil || created == nil || created.SandboxID != "sandbox-1" {
				t.Fatalf("create after VSOCK recovery = %+v, %v", created, err)
			}
			if attempts.Load() < 2 || manager.scheduledReconciles != 1 {
				t.Fatalf("dial attempts=%d pool reconciles=%d", attempts.Load(), manager.scheduledReconciles)
			}
		})
	}
}
