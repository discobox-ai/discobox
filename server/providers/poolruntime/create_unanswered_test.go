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
func newDroppingRuntimeProvider(t *testing.T, projectID, poolID, route string, drops int32, beforeDrop func()) (*testRuntimeProvider, *atomic.Int32) {
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
			if beforeDrop != nil {
				beforeDrop()
			}
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

	runtimeProvider, creates := newDroppingRuntimeProvider(t, "project-1", "pool-1", "/sandboxes", 2, nil)
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
	// Let the recovery budget expire before another attempt is due. Retry
	// counts and fresh leases are covered by the preceding test; this test
	// must not depend on fitting multiple HTTP requests into that budget.
	poolCapacityPollInterval = time.Hour
	poolAgentWaitCeiling = time.Second
	t.Cleanup(func() { poolCapacityPollInterval, poolAgentWaitCeiling = oldInterval, oldCeiling })

	runtimeProvider, creates := newDroppingRuntimeProvider(t, "project-1", "pool-1", "/sandboxes", 1<<30, nil)
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _, err := provider.Create(ctx, sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{PoolID: "pool-1"})
	if ctx.Err() != nil {
		t.Fatalf("create exhausted the outer guard instead of the recovery budget: %v", err)
	}
	if err == nil {
		t.Fatal("create on a pool agent that never answers succeeded")
	}
	if !strings.Contains(err.Error(), "create sandbox on pool pool-1") || strings.Contains(err.Error(), runtimeProvider.baseURL) {
		t.Fatalf("create error = %q, want the pool named and the agent's address left out", err)
	}
	if got := creates.Load(); got != 1 {
		t.Fatalf("creates sent = %d, want 1 before the recovery budget expires", got)
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

	runtimeProvider, _ := newDroppingRuntimeProvider(t, "project-1", "pool-1", "/sandboxes", 1<<30, nil)
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
			runtimeProvider, sent := newDroppingRuntimeProvider(t, "project-1", "pool-1", tc.route, 1, nil)
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
	runtimeProvider, _ := newDroppingRuntimeProvider(t, "project-1", "pool-1", "/start", 0, nil)
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

func TestLongDispatchedCreateGetsRecoveryFromItsFirstLoss(t *testing.T) {
	oldNow, oldPoll := poolWaitNow, poolCapacityPollInterval
	base := time.Now()
	var elapsed atomic.Int64
	poolWaitNow = func() time.Time { return base.Add(time.Duration(elapsed.Load())) }
	poolCapacityPollInterval = time.Millisecond
	t.Cleanup(func() { poolWaitNow = oldNow; poolCapacityPollInterval = oldPoll })
	runtimeProvider, creates := newDroppingRuntimeProvider(t, "project-1", "pool-1", "/sandboxes", 1, func() { elapsed.Store(int64(10 * time.Minute)) })
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, _, err := provider.Create(ctx, sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{PoolID: "pool-1"})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || creates.Load() != 2 {
		t.Fatalf("result=%v creates=%d", result, creates.Load())
	}
}

// A pool whose repair fails while it hosts sandboxes keeps serving them: its
// reconcile failure is an ErrorMessage on an active, ready pool, not a dead
// host (pools reconciler, failReconcile). A create lost to that repair is
// sent again to the agent that is still there, rather than settling the
// sandbox as failed with the pool's message while its box goes on to run.
func TestPoolProviderCreateIsSentAgainWhenThePoolRecordsARepairFailureMidRequest(t *testing.T) {
	oldInterval := poolCapacityPollInterval
	poolCapacityPollInterval = time.Millisecond
	t.Cleanup(func() { poolCapacityPollInterval = oldInterval })

	manager := &fakePoolManager{pool: hostingPool("pool-1"), schedulable: true}
	runtimeProvider, creates := newDroppingRuntimeProvider(t, "project-1", "pool-1", "/sandboxes", 1, recordRepairFailure(manager))
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)

	runtimeSandbox, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{
		PoolID: "pool-1",
	})
	if err != nil {
		t.Fatalf("create on a serving pool whose repair failed mid-request: %v", err)
	}
	if runtimeSandbox == nil || runtimeSandbox.SandboxID != "sandbox-1" {
		t.Fatalf("runtime sandbox = %#v", runtimeSandbox)
	}
	if got := creates.Load(); got != 2 {
		t.Fatalf("creates sent = %d, want 2: the dropped one and the one answered", got)
	}
}

// hostingPool is a pool that registered and has hosted sandboxes: what a pool
// repair runs on.
func hostingPool(id string) *model.Pool {
	pool := activePool(id)
	registered := time.Now().UTC().Add(-time.Hour)
	pool.RegisteredAt = &registered
	return pool
}

// recordRepairFailure records on the manager's pool what the pools reconciler
// records when a repair of a created pool fails (failReconcile): a message
// against the generation it attempted, state and readiness untouched.
func recordRepairFailure(manager *fakePoolManager) func() {
	return func() {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		message := "Error response from daemon: container is marked for removal and cannot be started; repair pool: Error response from daemon: container is marked for removal and cannot be started"
		settled := time.Now().UTC()
		manager.pool.ErrorMessage = &message
		manager.pool.ReconciledAt = &settled
	}
}

// The repair that failed is still replacing the pool's container, so its agent
// leaves the create unanswered for more than one pass. The recorded failure
// does not end the wait at any of them: a created pool's failure is never
// terminal, and the agent that comes back takes the create.
func TestPoolProviderCreateWaitsOutACreatedPoolsRecordedFailure(t *testing.T) {
	oldInterval := poolCapacityPollInterval
	poolCapacityPollInterval = time.Millisecond
	t.Cleanup(func() { poolCapacityPollInterval = oldInterval })

	manager := &fakePoolManager{pool: hostingPool("pool-1"), schedulable: true}
	runtimeProvider, creates := newDroppingRuntimeProvider(t, "project-1", "pool-1", "/sandboxes", 3, recordRepairFailure(manager))
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)

	runtimeSandbox, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{
		PoolID: "pool-1",
	})
	if err != nil {
		t.Fatalf("create on a created pool whose repair failed while its agent was away: %v", err)
	}
	if runtimeSandbox == nil || runtimeSandbox.SandboxID != "sandbox-1" {
		t.Fatalf("runtime sandbox = %#v", runtimeSandbox)
	}
	if got := creates.Load(); got != 4 {
		t.Fatalf("creates sent = %d, want 4: three dropped and the one answered", got)
	}
}

// A created pool's recorded failure does not end the wait, but a wait that
// runs out on a pool that never answered still says what the pool last
// recorded, beside what the last attempt was refused with.
func TestPoolProviderCreateOnACreatedPoolThatNeverAnswersSaysWhatItRecorded(t *testing.T) {
	oldTimeout, oldInterval := poolCapacityWaitTimeout, poolCapacityPollInterval
	poolCapacityWaitTimeout = 20 * time.Millisecond
	poolCapacityPollInterval = time.Millisecond
	t.Cleanup(func() { poolCapacityWaitTimeout, poolCapacityPollInterval = oldTimeout, oldInterval })

	pool := hostingPool("pool-1")
	message := "Cannot connect to the Docker daemon"
	pool.ErrorMessage = &message
	notRunning := errors.New(`pool "pool-1" container is exited`)
	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	runtimeProvider.unreachable = notRunning
	manager := &fakePoolManager{pool: pool, schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)

	_, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{PoolID: "pool-1"})
	if !errors.Is(err, notRunning) || !strings.Contains(err.Error(), message) {
		t.Fatalf("create error = %v, want %q and the pool's recorded %q", err, notRunning, message)
	}
	if runtimeProvider.acquireCalls < 2 {
		t.Fatalf("AcquirePoolAgentClient calls = %d, want the wait to keep trying", runtimeProvider.acquireCalls)
	}
}
