package poolruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	poolagent "github.com/discobox-ai/discobox/pool-agent"
	sandboxruntime "github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	poolagentserver "github.com/discobox-ai/discobox/pool-agent/server"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

// fakePoolManager implements sandbox.PoolManager over a single in-memory pool.
type fakePoolManager struct {
	pool                    *model.Pool
	schedulable             bool
	mintedBootstrapTokens   int
	agentTokenClaims        []poolagentauth.TokenClaims
	sandboxAgentTokenClaims []poolagentauth.TokenClaims
	scheduledReconciles     int
	scheduledPoolID         string
	scheduledRepairs        []string
	scheduleUnblocks        bool
}

func (m *fakePoolManager) GetPool(context.Context, string, string) (*model.Pool, error) {
	if m.pool == nil {
		return nil, apperrors.ErrNotFound
	}
	return m.pool, nil
}

func (m *fakePoolManager) ListPoolsForProviderInstance(context.Context, string, string) ([]model.Pool, error) {
	if m.pool == nil {
		return nil, nil
	}
	return []model.Pool{*m.pool}, nil
}

func (m *fakePoolManager) ListPools(context.Context, string) ([]model.Pool, error) {
	if m.pool == nil {
		return nil, nil
	}
	return []model.Pool{*m.pool}, nil
}

func (m *fakePoolManager) SchedulablePoolForSandbox(context.Context, *model.Sandbox) (*model.Pool, error) {
	if m.pool == nil || !m.schedulable {
		return nil, apperrors.ErrNotFound
	}
	return m.pool, nil
}

func (m *fakePoolManager) GetProject(context.Context, string) (*model.Project, error) {
	return &model.Project{ID: "project-1"}, nil
}

func (m *fakePoolManager) GetSandboxProviderInstance(context.Context, string, string) (*model.SandboxProviderInstance, error) {
	return &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}, nil
}

func (m *fakePoolManager) CountSandboxesForPool(context.Context, string, string) (int64, error) {
	return 0, nil
}

func (m *fakePoolManager) CreatePoolBootstrapToken(context.Context, *model.PoolBootstrapToken) error {
	m.mintedBootstrapTokens++
	return nil
}

func (m *fakePoolManager) EnsureAgentTrustKey(context.Context) (string, error) {
	return "control-plane-public-key", nil
}

func (m *fakePoolManager) CreateAgentToken(_ context.Context, claims poolagentauth.TokenClaims) (string, error) {
	m.agentTokenClaims = append(m.agentTokenClaims, claims)
	return "pool-token", nil
}

func (m *fakePoolManager) CreateSandboxAgentToken(_ context.Context, claims poolagentauth.TokenClaims) (string, error) {
	m.sandboxAgentTokenClaims = append(m.sandboxAgentTokenClaims, claims)
	return "sandbox-agent-token", nil
}

func (m *fakePoolManager) SchedulePoolReconciliation(_ context.Context, _, poolID string) error {
	m.scheduledReconciles++
	m.scheduledPoolID = poolID
	if m.scheduleUnblocks {
		m.schedulable = true
		if m.pool != nil {
			m.pool.ErrorMessage = nil
		}
	}
	return nil
}

func (m *fakePoolManager) SchedulePoolRepair(_ context.Context, poolID, _ string) error {
	m.scheduledRepairs = append(m.scheduledRepairs, poolID)
	return nil
}

func activePool(id string) *model.Pool {
	return &model.Pool{
		ID:        id,
		ProjectID: "project-1",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState: model.DesiredStatePresent,
			State:        model.PoolStateActive,
		},
		Ready:        true,
		Schedulable:  true,
		PoolManifest: model.PoolManifest{Name: id},
	}
}

func newTestRuntimeProvider(t *testing.T, projectID, poolID string) *testRuntimeProvider {
	t.Helper()
	runtime := sandboxruntime.NewMemorySandboxRuntime()
	controlPlaneKey, poolToken := newPoolAgentTestAuth(t, projectID, poolID)
	router, _ := poolagentserver.NewRouter(poolagentserver.Config{
		Identity:              poolagentserver.Identity{ProjectID: projectID, PoolID: poolID},
		Runtime:               runtime,
		ControlPlanePublicKey: controlPlaneKey,
	})
	agent := httptest.NewServer(router)
	t.Cleanup(agent.Close)
	return &testRuntimeProvider{baseURL: agent.URL, client: agent.Client(), token: poolToken, runtime: runtime}
}

type testRuntimeProvider struct {
	baseURL string
	client  *http.Client
	token   string
	runtime *sandboxruntime.MemorySandboxRuntime
	// createsRuntime makes EnsurePool mint, standing in for a provider that
	// actually creates a container. Left false, it stands in for a drift check
	// that finds a healthy runtime and needs no credentials.
	createsRuntime bool
	// staticToken returns a lease without a token provider, so the agent
	// client attaches its own claims-minting provider (asserted by tests).
	staticToken  bool
	acquireErrs  []error
	acquireCalls int
	consoleCalls int
}

func (p *testRuntimeProvider) Close() error { return nil }

func (p *testRuntimeProvider) EnsurePool(ctx context.Context, _ *model.Project, _ *model.SandboxProviderInstance, _ *model.Pool, mint poolagent.MintBootstrap) error {
	if !p.createsRuntime {
		return nil
	}
	_, err := mint(ctx)
	return err
}

func (p *testRuntimeProvider) RepairPool(context.Context, *model.Project, *model.SandboxProviderInstance, *model.Pool, poolagent.MintBootstrap, string) error {
	return nil
}

func (p *testRuntimeProvider) RemovePool(context.Context, *model.Project, *model.SandboxProviderInstance, *model.Pool) error {
	return nil
}

func (p *testRuntimeProvider) OpenConsole(context.Context, *model.SandboxProviderInstance, *model.Pool, sandbox.ConsoleOptions) (sandbox.PTY, error) {
	p.consoleCalls++
	return nil, errors.New("no console in unit tests")
}

func (p *testRuntimeProvider) AcquirePoolAgentClient(context.Context, *model.Pool) (*transport.HTTPClientLease, error) {
	p.acquireCalls++
	if len(p.acquireErrs) > 0 {
		err := p.acquireErrs[0]
		p.acquireErrs = p.acquireErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if p.staticToken {
		return transport.NewHTTPClientLeaseWithBaseURLAndAuth(p.client, p.baseURL, p.token, nil), nil
	}
	return transport.NewHTTPClientLeaseWithBaseURLAndAuthProvider(p.client, p.baseURL, func(context.Context) (string, error) {
		return p.token, nil
	}, nil), nil
}

func TestPoolProviderCreateCallsPoolAgentRuntime(t *testing.T) {
	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)

	runtimeSandbox, state, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{
		PoolID: "pool-1",
		Image:  sandbox.ImageRef{Name: "alpine:3.20"},
		Env:    map[string]string{"HELLO": "world"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if runtimeSandbox == nil || runtimeSandbox.SandboxID != "sandbox-1" || runtimeSandbox.Status != sandbox.StatusRunning || runtimeSandbox.Metadata["pool_id"] != "pool-1" {
		t.Fatalf("runtime sandbox = %#v", runtimeSandbox)
	}
	if len(state) == 0 {
		t.Fatal("expected provider state")
	}
	created, err := runtimeProvider.runtime.GetSandbox(context.Background(), "sandbox-1")
	if err != nil {
		t.Fatalf("pool runtime get sandbox: %v", err)
	}
	if created.Image != "alpine:3.20" || created.Env["HELLO"] != "world" {
		t.Fatalf("created sandbox = %#v", created)
	}
}

func TestPoolProviderCreateWaitsForSchedulablePool(t *testing.T) {
	oldTimeout := poolCapacityWaitTimeout
	oldInterval := poolCapacityPollInterval
	poolCapacityWaitTimeout = 50 * time.Millisecond
	poolCapacityPollInterval = time.Millisecond
	t.Cleanup(func() {
		poolCapacityWaitTimeout = oldTimeout
		poolCapacityPollInterval = oldInterval
	})

	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	pool := activePool("pool-1")
	pool.Ready = false
	pool.Schedulable = false
	pool.ObservedGeneration = pool.Generation - 1 // a retry is pending
	manager := &fakePoolManager{pool: pool, schedulable: false, scheduleUnblocks: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)

	runtimeSandbox, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{PoolID: "pool-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if manager.scheduledReconciles == 0 {
		t.Fatal("expected pool reconcile to be scheduled for missing capacity")
	}
	if runtimeSandbox == nil || runtimeSandbox.Metadata["pool_id"] != "pool-1" {
		t.Fatalf("runtime sandbox = %#v, want pool-1", runtimeSandbox)
	}
}

func TestPoolProviderCreateSurfacesSettledPoolFailure(t *testing.T) {
	oldTimeout := poolCapacityWaitTimeout
	oldInterval := poolCapacityPollInterval
	poolCapacityWaitTimeout = 50 * time.Millisecond
	poolCapacityPollInterval = time.Millisecond
	t.Cleanup(func() {
		poolCapacityWaitTimeout = oldTimeout
		poolCapacityPollInterval = oldInterval
	})

	message := "No such image: discobox-pool-agent:latest"
	pool := activePool("pool-1")
	pool.Ready = false
	pool.Schedulable = false
	pool.ErrorMessage = &message
	manager := &fakePoolManager{pool: pool, schedulable: false}
	provider := New(newTestRuntimeProvider(t, "project-1", "pool-1"), sandbox.ProviderDefinition{Name: "test"}, manager)

	_, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{PoolID: "pool-1"})
	if !errors.Is(err, sandbox.ErrNoSandboxCapacity) {
		t.Fatalf("create error = %v, want ErrNoSandboxCapacity", err)
	}
	if !strings.Contains(err.Error(), message) {
		t.Fatalf("create error = %v, want the pool's recorded cause", err)
	}
}

func TestPoolProviderCreateWithExistingStateReusesPool(t *testing.T) {
	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: false}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)

	state := poolRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Image: "pool-runtime", Metadata: map[string]string{"pool_id": "pool-1"}})
	runtimeSandbox, nextState, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, state, sandbox.CreateOptions{PoolID: "pool-1"})
	if err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	if runtimeSandbox == nil || runtimeSandbox.Metadata["pool_id"] != "pool-1" {
		t.Fatalf("runtime sandbox = %#v, want pool-1 metadata", runtimeSandbox)
	}
	if gotPoolID, err := poolIDFromRuntimeState(nextState); err != nil || gotPoolID != "pool-1" {
		t.Fatalf("next state pool ID = %q, %v; want pool-1", gotPoolID, err)
	}
}

func TestPoolProviderCreateRejectsStateFromOtherPool(t *testing.T) {
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(newTestRuntimeProvider(t, "project-1", "pool-1"), sandbox.ProviderDefinition{Name: "test"}, manager)

	state := poolRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Metadata: map[string]string{"pool_id": "pool-2"}})
	_, _, err := provider.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, state, sandbox.CreateOptions{PoolID: "pool-1"})
	if !errors.Is(err, sandbox.ErrNoSandboxCapacity) {
		t.Fatalf("create error = %v, want ErrNoSandboxCapacity", err)
	}
}

// TestPoolProviderMintsBootstrapOnlyWhenRuntimeIsCreated pins the fix for an
// unbounded token leak: minting persists a single-use bootstrap token, and the
// reconcile drift-checks every healthy pool, so minting eagerly wrote one
// token row per reconcile.
func TestPoolProviderMintsBootstrapOnlyWhenRuntimeIsCreated(t *testing.T) {
	project := &model.Project{ID: "project-1"}
	providerInstance := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}
	pool := activePool("pool-1")
	registeredAt := time.Now().UTC()
	pool.RegisteredAt = &registeredAt

	t.Run("drift check over a healthy runtime", func(t *testing.T) {
		manager := &fakePoolManager{pool: pool}
		provider := New(&testRuntimeProvider{}, sandbox.ProviderDefinition{Name: "test"}, manager)
		if err := provider.ReconcilePool(context.Background(), manager, project, providerInstance, pool); err != nil {
			t.Fatalf("reconcile pool: %v", err)
		}
		if manager.mintedBootstrapTokens != 0 {
			t.Fatalf("minted %d bootstrap tokens, want none when no runtime is created", manager.mintedBootstrapTokens)
		}
	})

	t.Run("runtime creation", func(t *testing.T) {
		manager := &fakePoolManager{pool: pool}
		provider := New(&testRuntimeProvider{createsRuntime: true}, sandbox.ProviderDefinition{Name: "test"}, manager)
		if err := provider.ReconcilePool(context.Background(), manager, project, providerInstance, pool); err != nil {
			t.Fatalf("reconcile pool: %v", err)
		}
		if manager.mintedBootstrapTokens != 1 {
			t.Fatalf("minted %d bootstrap tokens, want exactly 1 for a created runtime", manager.mintedBootstrapTokens)
		}
	})
}

func TestPoolProviderAcquireHTTPClientReconcilesPoolAndRetries(t *testing.T) {
	oldTimeout := poolCapacityWaitTimeout
	oldInterval := poolCapacityPollInterval
	poolCapacityWaitTimeout = 50 * time.Millisecond
	poolCapacityPollInterval = time.Millisecond
	t.Cleanup(func() {
		poolCapacityWaitTimeout = oldTimeout
		poolCapacityPollInterval = oldInterval
	})

	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	runtimeProvider.staticToken = true
	runtimeProvider.acquireErrs = []error{sandbox.ErrNotFound}
	manager := &fakePoolManager{pool: activePool("pool-1")}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)
	state := poolRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Metadata: map[string]string{"pool_id": "pool-1"}})

	lease, err := provider.AcquireHTTPClient(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, state, []string{poolagentauth.ScopeSandboxRead})
	if err != nil {
		t.Fatalf("acquire HTTP client: %v", err)
	}
	defer lease.Release()
	if manager.scheduledReconciles != 1 {
		t.Fatalf("scheduled pool reconciles = %d, want 1", manager.scheduledReconciles)
	}
	if runtimeProvider.acquireCalls != 2 {
		t.Fatalf("AcquirePoolAgentClient calls = %d, want 2", runtimeProvider.acquireCalls)
	}
	if _, err := lease.AuthorizationToken(context.Background()); err != nil {
		t.Fatalf("lease authorization token: %v", err)
	}
	if len(manager.agentTokenClaims) != 1 {
		t.Fatalf("agent token claims count = %d, want 1", len(manager.agentTokenClaims))
	}
	if claims := manager.agentTokenClaims[0]; claims.ProjectID != "project-1" || claims.PoolID != "pool-1" || claims.SandboxID != "sandbox-1" || !reflect.DeepEqual(claims.Scopes, []string{poolagentauth.ScopeSandboxRead}) {
		t.Fatalf("agent token claims = %#v", claims)
	}
}

func TestPoolFailureUnwrapsToNoCapacity(t *testing.T) {
	err := error(&sandbox.PoolFailure{PoolID: "pool-1", Message: "No such image"})

	if !errors.Is(err, sandbox.ErrNoSandboxCapacity) {
		t.Fatalf("errors.Is(%v, ErrNoSandboxCapacity) = false, want true", err)
	}
	if got, want := err.Error(), "pool pool-1 failed: No such image"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func poolRuntimeState(t *testing.T, runtimeSandbox *sandbox.Sandbox) []byte {
	t.Helper()
	state, err := json.Marshal(runtimeSandbox)
	if err != nil {
		t.Fatalf("marshal pool runtime state: %v", err)
	}
	return state
}
