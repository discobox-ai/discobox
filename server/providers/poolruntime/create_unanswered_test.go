package poolruntime

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	sandboxruntime "github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	poolagentserver "github.com/discobox-ai/discobox/pool-agent/server"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

// newDroppingRuntimeProvider is a pool agent that closes the connection under
// the first drops POSTs to a path ending in route without answering them, the
// way one whose container is replaced mid-request does, and counts them.
func newDroppingRuntimeProvider(t *testing.T, projectID, poolID, route string, drops int32) (*testRuntimeProvider, *atomic.Int32) {
	t.Helper()
	runtime := sandboxruntime.NewMemorySandboxRuntime()
	controlPlaneKey, poolToken := newPoolAgentTestAuth(t, projectID, poolID, poolagentserver.ScopeSandboxRead, poolagentserver.ScopeSandboxWrite)
	router, _ := poolagentserver.NewRouter(poolagentserver.Config{
		Identity:              poolagentserver.Identity{ProjectID: projectID, PoolID: poolID},
		Runtime:               runtime,
		ControlPlanePublicKey: controlPlaneKey,
	})
	var sent atomic.Int32
	agent := newCreateProtocolTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, route) && sent.Add(1) <= drops {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack %s: %v", route, err)
				return
			}
			_ = conn.Close()
			return
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(agent.Close)
	return &testRuntimeProvider{baseURL: agent.URL, client: agent.Client(), token: poolToken, runtime: runtime}, &sent
}

// A pool agent replaced while a create is in it closes the connection without
// answering. That is the agent leaving, not the create's verdict: the create is
// sent again to the agent that comes back, rather than settling the sandbox as
// failed with its old container already removed.
func TestPoolProviderCreateIsSentAgainWhenThePoolAgentLeavesMidRequest(t *testing.T) {
	oldInterval := poolCapacityPollInterval
	poolCapacityPollInterval = time.Millisecond
	t.Cleanup(func() { poolCapacityPollInterval = oldInterval })

	runtimeProvider, creates := newDroppingRuntimeProvider(t, "project-1", "pool-1", "/sandboxes", 2)
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)

	runtimeSandbox, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{
		PoolID: "pool-1",
	})
	if err != nil {
		t.Fatalf("create across a pool agent that left mid-request: %v", err)
	}
	if runtimeSandbox == nil || runtimeSandbox.SandboxID != "sandbox-1" {
		t.Fatalf("runtime sandbox = %#v", runtimeSandbox)
	}
	if got := creates.Load(); got != 3 {
		t.Fatalf("creates sent = %d, want 3: two dropped and the one answered", got)
	}
	if runtimeProvider.acquireCalls != 3 {
		t.Fatalf("AcquirePoolAgentClient calls = %d, want 3: every attempt needs its own lease", runtimeProvider.acquireCalls)
	}
}

// An agent that never answers the create is a failure, reported without the
// agent's internal address, once the ceiling the host wait has runs out.
func TestPoolProviderCreateGivesUpOnAPoolAgentThatKeepsLeaving(t *testing.T) {
	oldInterval, oldCeiling := poolCapacityPollInterval, poolAgentWaitCeiling
	poolCapacityPollInterval = time.Millisecond
	poolAgentWaitCeiling = 20 * time.Millisecond
	t.Cleanup(func() { poolCapacityPollInterval, poolAgentWaitCeiling = oldInterval, oldCeiling })

	runtimeProvider, creates := newDroppingRuntimeProvider(t, "project-1", "pool-1", "/sandboxes", 1<<30)
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)

	_, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{PoolID: "pool-1"})
	if err == nil {
		t.Fatal("create on a pool agent that never answers succeeded")
	}
	if !strings.Contains(err.Error(), "create sandbox on pool pool-1") || strings.Contains(err.Error(), runtimeProvider.baseURL) {
		t.Fatalf("create error = %q, want the pool named and the agent's address left out", err)
	}
	if creates.Load() < 2 {
		t.Fatalf("creates sent = %d, want it tried again before giving up", creates.Load())
	}
}

// An answer is a verdict whatever its status: only a request the agent never
// answered is sent again.
func TestPoolProviderCreateDoesNotRetryAnAnsweredRefusal(t *testing.T) {
	var creates atomic.Int32
	agent := newCreateProtocolTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only creates are counted: every listener opened in a discobox is
		// probed once (root REVIEW.md), and a probe is not a create.
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/sandboxes") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		creates.Add(1)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"title":"Internal Server Error","status":500,"detail":"no such image"}`))
	}))
	t.Cleanup(agent.Close)
	runtimeProvider := &testRuntimeProvider{baseURL: agent.URL, client: agent.Client(), token: "token"}
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)

	_, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{PoolID: "pool-1"})
	if err == nil {
		t.Fatal("create the pool agent refused succeeded")
	}
	if got := creates.Load(); got != 1 {
		t.Fatalf("creates sent = %d, want 1: a refusal is an answer (%v)", got, err)
	}
}

// Only an agent that was not there to answer is asked again. The client hands
// back a *url.Error for failures no agent coming back fixes, and for a timeout
// of its own under which the agent may still be building the sandbox.
func TestPoolAgentUnansweredIsOnlyAnAgentThatWasNotThere(t *testing.T) {
	post := func(err error) error { return &url.Error{Op: "Post", URL: "http://pool/sandboxes", Err: err} }
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"closed under the request", post(io.EOF), true},
		{"closed idle connection during agent restart", post(errors.New("http: server closed idle connection")), true},
		{"closed mid-response", post(io.ErrUnexpectedEOF), true},
		{"refused", post(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}), true},
		{"reset", post(&net.OpError{Op: "read", Err: syscall.ECONNRESET}), true},
		{"untrusted certificate", post(x509.UnknownAuthorityError{}), false},
		{"client timeout", post(context.DeadlineExceeded), false},
		{"unparseable address", &url.Error{Op: "parse", URL: "::", Err: errors.New("missing protocol scheme")}, false},
		{"an answer", errors.New("pool-agent request failed: no such image"), false},
		{"no error", nil, false},
	} {
		if got := poolAgentUnanswered(context.Background(), tc.err); got != tc.want {
			t.Errorf("%s: unanswered = %v, want %v", tc.name, got, tc.want)
		}
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if poolAgentUnanswered(gone, post(io.EOF)) {
		t.Error("a caller that stopped waiting was taken for the agent leaving")
	}
}

// downAfterFirstAcquire is a pool host that is there for the first create and
// gone from then on.
type downAfterFirstAcquire struct {
	*testRuntimeProvider
	acquires atomic.Int32
}

func (p *downAfterFirstAcquire) AcquirePoolAgentClient(ctx context.Context, pool *model.Pool) (*transport.HTTPClientLease, error) {
	if p.acquires.Add(1) > 1 {
		return nil, fmt.Errorf("%w: pool host is gone", sandbox.ErrPoolNotReachable)
	}
	return p.testRuntimeProvider.AcquirePoolAgentClient(ctx, pool)
}

// A lost request is the account of a host that never came back in time, and
// of nothing else: a pool deleted while its agent was away fails the create
// for that, not for the connection that closed before it.
func TestPoolProviderCreateReportsWhatEndedTheWaitAfterALostRequest(t *testing.T) {
	oldInterval := poolCapacityPollInterval
	poolCapacityPollInterval = time.Millisecond
	t.Cleanup(func() { poolCapacityPollInterval = oldInterval })

	runtimeProvider, _ := newDroppingRuntimeProvider(t, "project-1", "pool-1", "/sandboxes", 1<<30)
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	// The pool is deleted while its agent is away: the wait for the host
	// reads it gone.
	provider := New(&downAfterFirstAcquire{testRuntimeProvider: runtimeProvider}, sandbox.ProviderDefinition{Name: "test"}, &deletedOnReconcile{fakePoolManager: manager}, nil)

	_, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{PoolID: "pool-1"})
	if !errors.Is(err, apperrors.ErrNotFound) || strings.Contains(err.Error(), "EOF") {
		t.Fatalf("create on a pool deleted while its agent was away = %v, want the pool not found and no EOF", err)
	}
}

// deletedOnReconcile is a pool whose row is gone by the time anything asks
// for it to be reconciled.
type deletedOnReconcile struct {
	*fakePoolManager
}

func (m *deletedOnReconcile) SchedulePoolReconciliation(ctx context.Context, projectID, poolID string) error {
	m.mu.Lock()
	m.pool = nil
	m.mu.Unlock()
	return m.fakePoolManager.SchedulePoolReconciliation(ctx, projectID, poolID)
}

// createdOn creates sandbox-1 on the pool through provider and returns its
// runtime state, which names the pool every later instruction is sent to.
func createdOn(t *testing.T, provider *Provider) []byte {
	t.Helper()
	_, state, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{PoolID: "pool-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return state
}

// A server upgrade replaces every pool's agent, and the first thing sent to
// it is often a person's start. The agent leaving under an instruction is not
// its answer, and is not reported as one with only "EOF" to say why
// (discobox#544): it is the pool not being reachable, which the caller that
// can wait waits out. The instruction itself is sent once.
func TestPoolProviderInstructionLostToTheAgentLeavingIsThePoolNotReachable(t *testing.T) {
	ref := sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}
	for _, tc := range []struct {
		route string
		send  func(*Provider, []byte) ([]byte, error)
	}{
		{"/start", func(p *Provider, state []byte) ([]byte, error) { return p.Start(context.Background(), ref, state) }},
		{"/stop", func(p *Provider, state []byte) ([]byte, error) {
			return p.Stop(context.Background(), ref, state, time.Second)
		}},
		{"/restart", func(p *Provider, state []byte) ([]byte, error) {
			return p.Restart(context.Background(), ref, state, time.Second)
		}},
		{"/suspend", func(p *Provider, state []byte) ([]byte, error) { return p.Suspend(context.Background(), ref, state) }},
		{"/ensure-running", func(p *Provider, state []byte) ([]byte, error) {
			return state, p.EnsureRunning(context.Background(), ref, state)
		}},
	} {
		t.Run(strings.TrimPrefix(tc.route, "/"), func(t *testing.T) {
			runtimeProvider, sent := newDroppingRuntimeProvider(t, "project-1", "pool-1", tc.route, 1)
			manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
			provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
			state := createdOn(t, provider)

			rotated, err := tc.send(provider, state)
			if !errors.Is(err, sandbox.ErrPoolNotReachable) {
				t.Fatalf("%s lost to the agent leaving = %v, want the pool not reachable", tc.route, err)
			}
			if !strings.Contains(err.Error(), "sandbox on pool pool-1") || strings.Contains(err.Error(), runtimeProvider.baseURL) {
				t.Fatalf("error = %q, want the pool named and the agent's address left out", err)
			}
			if string(rotated) != string(state) {
				t.Fatalf("%s returned state %q, want the state it was given", tc.route, rotated)
			}
			if got := sent.Load(); got != 1 {
				t.Fatalf("%s sent = %d, want 1: an instruction is not resent here", tc.route, got)
			}
		})
	}
}

// A refusal is an answer, and keeps saying what it is.
func TestPoolProviderInstructionRefusalIsNotThePoolNotReachable(t *testing.T) {
	runtimeProvider, _ := newDroppingRuntimeProvider(t, "project-1", "pool-1", "/start", 0)
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
	state := createdOn(t, provider)

	// The agent holds no such sandbox, and says so.
	_, err := provider.Start(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-2"}, state)
	if err == nil || errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Fatalf("start of a sandbox the agent does not hold = %v, want its refusal", err)
	}
}

// A retry whose request is still in flight when the recovery budget expires
// must report the same pool-level failure as expiry between attempts.
func TestPoolProviderCreateReportsPoolWhenRetryExpiresInFlight(t *testing.T) {
	oldInterval, oldCeiling := poolCapacityPollInterval, poolAgentWaitCeiling
	poolCapacityPollInterval = time.Millisecond
	poolAgentWaitCeiling = 100 * time.Millisecond
	t.Cleanup(func() { poolCapacityPollInterval, poolAgentWaitCeiling = oldInterval, oldCeiling })

	var creates atomic.Int32
	agent := newCreateProtocolTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/sandboxes") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if creates.Add(1) > 1 {
			// Hold the retry beyond its context deadline before dropping it.
			time.Sleep(200 * time.Millisecond)
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(agent.Close)
	runtimeProvider := &testRuntimeProvider{baseURL: agent.URL, client: agent.Client()}
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
	_, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{PoolID: "pool-1"})
	if creates.Load() < 2 {
		t.Fatalf("creates sent = %d, want at least two", creates.Load())
	}
	if err == nil || !strings.Contains(err.Error(), "create sandbox on pool pool-1") || strings.Contains(err.Error(), "http://pool") {
		t.Fatalf("create error = %q, want the pool named and the agent address left out", err)
	}
}
