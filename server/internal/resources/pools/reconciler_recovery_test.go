package pools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/server/internal/transport"
	"gorm.io/gorm"
)

// TestReconcileClearsARecordedFailureOnSuccess pins the half of the pool's
// status the reconciler owns. ErrorMessage used to be a one-way latch: a
// successful reconcile re-read the row, saw the error from the attempt it had
// just superseded, and returned before reaching the clear. The pool then
// reported a failure forever — indefinitely, and with text describing a
// condition that no longer existed.
func TestReconcileClearsARecordedFailureOnSuccess(t *testing.T) {
	ctx := context.Background()
	appStore, _ := newPoolReconcilerTestStore(t)
	manager := sandbox.NewProviderManager()
	manager.RegisterProvider("stub", stubPoolProvider{})

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "stub", Name: "stub"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	registeredAt := time.Now().UTC()
	pool := &model.Pool{
		ID:           "pool-1",
		ProjectID:    "project-1",
		PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: provider.ID},
		Ready:        true,
		Schedulable:  true,
		RegisteredAt: &registeredAt,
		LastSeenAt:   &registeredAt,
	}
	pool.DesiredState = model.DesiredStatePresent
	// The state an earlier failure left behind: offline with the reason
	// recorded against its generation (the shape a stale-heartbeat verdict
	// writes, and what pre-liveness-semantics failures wrote).
	pool.RecordFailure(model.PoolStateOffline, "runtime did not converge", "")
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	reconciler := NewPoolReconciler(appStore, manager, NewControlPlane(appStore, nil))
	if _, err := reconciler.Reconcile(ctx, PoolDirtyID(pool.ProjectID, pool.ID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	updated, err := appStore.GetPool(ctx, pool.ProjectID, pool.ID)
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if updated.ErrorMessage != nil {
		t.Fatalf("error message = %q, want none: the reconcile that just succeeded disproves it", *updated.ErrorMessage)
	}
	if updated.State != model.PoolStateActive {
		t.Fatalf("state = %q, want %q", updated.State, model.PoolStateActive)
	}
	if !updated.Converged() {
		t.Fatalf("generations = %d/%d, want converged", updated.ObservedGeneration, updated.Generation)
	}
}

// TestReconcilePromotesAPoolThatRegisteredAfterConverging pins the derivation
// that replaced carrying State forward. The create reconcile converges the
// generation while the pool is still `registering`, so by the time the agent
// calls home the pool is already "successful" and every later pass is a drift
// re-check. A re-check that preserved the recorded state would leave a
// registered, heartbeating pool reading `registering` forever — the promotion
// only used to happen because registration wrote `active` itself, which is the
// API layer writing the reconciler's fields.
func TestReconcilePromotesAPoolThatRegisteredAfterConverging(t *testing.T) {
	ctx := context.Background()
	appStore, _ := newPoolReconcilerTestStore(t)
	manager := sandbox.NewProviderManager()
	manager.RegisterProvider("stub", stubPoolProvider{})

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "stub", Name: "stub"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	registeredAt := time.Now().UTC()
	pool := &model.Pool{
		ID:           "pool-1",
		ProjectID:    "project-1",
		PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: provider.ID},
		RegisteredAt: &registeredAt,
		LastSeenAt:   &registeredAt,
	}
	pool.DesiredState = model.DesiredStatePresent
	pool.SetState(model.PoolStateRegistering)
	// Converged with no error: the runtime came up and this generation is done.
	pool.ObservedGeneration = pool.Generation
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	reconciler := NewPoolReconciler(appStore, manager, NewControlPlane(appStore, nil))
	if _, err := reconciler.Reconcile(ctx, PoolDirtyID(pool.ProjectID, pool.ID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	updated, err := appStore.GetPool(ctx, pool.ProjectID, pool.ID)
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if updated.State != model.PoolStateActive {
		t.Fatalf("state = %q, want %q: the agent has registered", updated.State, model.PoolStateActive)
	}
}

// TestFailedReconcileKeepsALiveCreatedPoolActive pins the offline/degraded
// split: a created pool whose agent is heartbeating stays `active` when a
// reconcile fails — the failure lands as ErrorMessage, and the agent-owned
// health flags are left alone. Spelling every failed convergence as `offline`
// is what made a live, serving pool read as unreachable (and blocked traffic
// onto its sandboxes) over an image-sync error that only affects future
// creates.
func TestFailedReconcileKeepsALiveCreatedPoolActive(t *testing.T) {
	ctx := context.Background()
	appStore, _ := newPoolReconcilerTestStore(t)
	manager := sandbox.NewProviderManager()
	manager.RegisterProvider("stub", failingPoolProvider{stubPoolProvider{}})

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "stub", Name: "stub"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	now := time.Now().UTC()
	pool := &model.Pool{
		ID:           "pool-1",
		ProjectID:    "project-1",
		PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: provider.ID},
		Ready:        true,
		Schedulable:  true,
		RegisteredAt: &now,
		LastSeenAt:   &now,
	}
	pool.DesiredState = model.DesiredStatePresent
	pool.SetState(model.PoolStateActive)
	pool.ObservedGeneration = pool.Generation
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	reconciler := NewPoolReconciler(appStore, manager, NewControlPlane(appStore, nil))
	if _, err := reconciler.Reconcile(ctx, PoolDirtyID(pool.ProjectID, pool.ID)); err == nil {
		t.Fatal("reconcile succeeded, want the provider's failure")
	}

	updated, err := appStore.GetPool(ctx, pool.ProjectID, pool.ID)
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if updated.State != model.PoolStateActive {
		t.Fatalf("state = %q, want %q: a heartbeating pool with a failing reconcile is degraded, not offline", updated.State, model.PoolStateActive)
	}
	if updated.ErrorMessage == nil || *updated.ErrorMessage != "runtime did not converge" {
		t.Fatalf("error message = %v, want the reconcile failure recorded", updated.ErrorMessage)
	}
	if !updated.Ready || !updated.Schedulable {
		t.Fatalf("ready=%t schedulable=%t, want the agent's flags left alone", updated.Ready, updated.Schedulable)
	}
	if !updated.Converged() {
		t.Fatalf("generations = %d/%d, want the failure attributed to the attempted generation", updated.ObservedGeneration, updated.Generation)
	}
}

// TestStaleHeartbeatReadsOffline pins what `offline` now means (ADR 0017 §4):
// the agent stopped answering. The runtime converges fine — the container is
// there — but the last heartbeat is past the timeout, so the reconciler's
// verdict is offline, with a freshly derived message. A resumed heartbeat plus
// the reconcile it triggers reads the pool back to active.
func TestStaleHeartbeatReadsOffline(t *testing.T) {
	ctx := context.Background()
	appStore, _ := newPoolReconcilerTestStore(t)
	manager := sandbox.NewProviderManager()
	manager.RegisterProvider("stub", stubPoolProvider{})

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "stub", Name: "stub"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	registeredAt := time.Now().UTC().Add(-time.Hour)
	stale := time.Now().UTC().Add(-2 * poolHeartbeatTimeout)
	pool := &model.Pool{
		ID:           "pool-1",
		ProjectID:    "project-1",
		PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: provider.ID},
		Ready:        true,
		Schedulable:  true,
		RegisteredAt: &registeredAt,
		LastSeenAt:   &stale,
	}
	pool.DesiredState = model.DesiredStatePresent
	pool.SetState(model.PoolStateActive)
	pool.ObservedGeneration = pool.Generation
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	reconciler := NewPoolReconciler(appStore, manager, NewControlPlane(appStore, nil))
	if _, err := reconciler.Reconcile(ctx, PoolDirtyID(pool.ProjectID, pool.ID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	updated, err := appStore.GetPool(ctx, pool.ProjectID, pool.ID)
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if updated.State != model.PoolStateOffline {
		t.Fatalf("state = %q, want %q: the agent has not heartbeated within the timeout", updated.State, model.PoolStateOffline)
	}
	if updated.ErrorMessage == nil || !strings.Contains(*updated.ErrorMessage, "has not reported") {
		t.Fatalf("error message = %v, want the staleness cause", updated.ErrorMessage)
	}

	// The agent comes back: a heartbeat refreshes LastSeenAt, and the
	// reconcile it triggers proves recovery.
	if _, err := appStore.UpdatePoolStatus(ctx, pool.ID, true, true, false, 1, 1<<30, 1<<30, nil); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, PoolDirtyID(pool.ProjectID, pool.ID)); err != nil {
		t.Fatalf("reconcile after recovery: %v", err)
	}
	recovered, err := appStore.GetPool(ctx, pool.ProjectID, pool.ID)
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if recovered.State != model.PoolStateActive {
		t.Fatalf("state = %q, want %q after the agent resumed heartbeating", recovered.State, model.PoolStateActive)
	}
	if recovered.ErrorMessage != nil {
		t.Fatalf("error message = %q, want none after recovery", *recovered.ErrorMessage)
	}
}

// failingPoolProvider converges nothing: every pool reconcile fails the same
// way, standing in for a runtime step that keeps losing (an unpullable image,
// an unreachable daemon).
type failingPoolProvider struct{ stubPoolProvider }

func (failingPoolProvider) ReconcilePool(context.Context, sandbox.PoolManager, *model.Project, *model.SandboxProviderInstance, *model.Pool, []string, func(context.Context) error) error {
	return errors.New("runtime did not converge")
}

func newPoolReconcilerTestStore(t *testing.T) (*store.Store, *gorm.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	return store.New(db.Write, db.Read), db.Write
}

// stubPoolProvider is a provider whose pool runtime always converges. Only the
// PoolRuntime half is exercised; the sandbox surface is present
// because the reconciler resolves providers through sandbox.Provider.
// A pool container whose healthcheck has not passed yet is a host on its way
// up, not a broken one, and the reconcile reports it as such rather than
// failing. A failed reconcile of a pool with sandboxes assigned repairs it —
// removing and recreating the container, restarting the very healthcheck
// something is waiting on (ADR 0039 tier 1) — and a pool container restarts
// for ordinary reasons: a Docker restart, a development image rebuild.
func TestReconcileRequeuesAPoolHostThatIsStillComingUp(t *testing.T) {
	ctx := context.Background()
	appStore, _ := newPoolReconcilerTestStore(t)
	runtime := &hostComingUpProvider{}
	manager := sandbox.NewProviderManager()
	manager.RegisterProvider("stub", runtime)

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "stub", Name: "stub"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	registeredAt := time.Now().UTC()
	pool := &model.Pool{
		ID:           "pool-1",
		ProjectID:    "project-1",
		PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: provider.ID},
		Ready:        true,
		Schedulable:  true,
		RegisteredAt: &registeredAt,
		LastSeenAt:   &registeredAt,
	}
	pool.DesiredState = model.DesiredStatePresent
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	// The case that matters: a sandbox is assigned, so a failed reconcile is
	// the path that repairs the host rather than recording the failure.
	if err := appStore.CreateSandbox(ctx, &model.Sandbox{
		ID: "sb-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "sb-1",
		ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.DesiredStatePresent, State: model.SandboxStateReady},
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if assigned, err := appStore.CountSandboxesForPool(ctx, "project-1", "pool-1"); err != nil || assigned == 0 {
		t.Fatalf("assigned sandboxes = %d (err %v), want at least one or this test proves nothing", assigned, err)
	}

	before, err := appStore.GetPool(ctx, pool.ProjectID, pool.ID)
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}

	reconciler := NewPoolReconciler(appStore, manager, NewControlPlane(appStore, nil))
	result, err := reconciler.Reconcile(ctx, PoolDirtyID(pool.ProjectID, pool.ID))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.RequeueAt.IsZero() {
		t.Fatal("a host that is still coming up did not ask to be looked at again")
	}
	if runtime.repairs != 0 {
		t.Fatalf("repairs = %d, want none: a repair restarts the healthcheck being waited on", runtime.repairs)
	}
	updated, err := appStore.GetPool(ctx, pool.ProjectID, pool.ID)
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if updated.ErrorMessage != nil {
		t.Fatalf("error message = %q, want none: a host coming up is not a failure", *updated.ErrorMessage)
	}
	// Settles nothing: the pass that found a host coming up leaves the row as
	// it was and asks again, rather than advancing this generation over a
	// runtime it never reached.
	if updated.State != before.State {
		t.Fatalf("state = %q, want %q unchanged", updated.State, before.State)
	}
	if updated.ObservedGeneration != before.ObservedGeneration {
		t.Fatalf("observed generation = %d, want %d unchanged", updated.ObservedGeneration, before.ObservedGeneration)
	}
}

// hostComingUpProvider is a pool runtime whose host is up but not yet taking
// traffic — the verdict dockerworker.PoolAgentUnreachable returns for a
// container whose healthcheck has not passed.
type hostComingUpProvider struct {
	stubPoolProvider
	repairs int
}

func (p *hostComingUpProvider) ReconcilePool(context.Context, sandbox.PoolManager, *model.Project, *model.SandboxProviderInstance, *model.Pool, []string, func(context.Context) error) error {
	return fmt.Errorf("%w: container abc health check is starting", sandbox.ErrPoolNotReachable)
}

func (p *hostComingUpProvider) RepairPool(context.Context, sandbox.PoolManager, *model.Project, *model.SandboxProviderInstance, *model.Pool, string, []string, func(context.Context) error) error {
	p.repairs++
	return nil
}

type stubPoolProvider struct{}

func (stubPoolProvider) ReconcilePool(context.Context, sandbox.PoolManager, *model.Project, *model.SandboxProviderInstance, *model.Pool, []string, func(context.Context) error) error {
	return nil
}

func (stubPoolProvider) RepairPool(context.Context, sandbox.PoolManager, *model.Project, *model.SandboxProviderInstance, *model.Pool, string, []string, func(context.Context) error) error {
	return nil
}

func (stubPoolProvider) RemovePool(context.Context, sandbox.PoolManager, *model.Project, *model.SandboxProviderInstance, *model.Pool) error {
	return nil
}

func (stubPoolProvider) ClearCache(context.Context, *model.Pool) ([]string, error) {
	return nil, nil
}

func (stubPoolProvider) OpenConsole(context.Context, *model.SandboxProviderInstance, *model.Pool, sandbox.ConsoleOptions) (sandbox.PTY, error) {
	return nil, errors.New("no console in unit tests")
}

func (stubPoolProvider) OpenLogs(context.Context, *model.SandboxProviderInstance, *model.Pool, sandbox.PoolLogOptions) (*sandbox.PoolLogStream, error) {
	return nil, errors.New("no host log in unit tests")
}

func (stubPoolProvider) BuildGuestImage(context.Context, *model.SandboxProviderInstance, *model.Pool, sandbox.GuestImageBuildOptions) (*sandbox.GuestImageBuild, error) {
	return nil, sandbox.ErrGuestImageBuildUnsupported
}

func (stubPoolProvider) Initialize(context.Context, *model.SandboxProviderInstance) error { return nil }
func (stubPoolProvider) Close() error                                                     { return nil }
func (stubPoolProvider) Definition() sandbox.ProviderDefinition {
	return sandbox.ProviderDefinition{Name: "Stub"}
}
func (stubPoolProvider) Status() sandbox.ProviderStatus {
	return sandbox.ProviderStatus{Available: true}
}
func (stubPoolProvider) Reconcile(context.Context) error                  { return nil }
func (stubPoolProvider) RemoveProject(context.Context, string) error      { return nil }
func (stubPoolProvider) List(context.Context) ([]*sandbox.Sandbox, error) { return nil, nil }

func (stubPoolProvider) Create(context.Context, sandbox.SandboxRef, []byte, sandbox.CreateOptions) (*sandbox.Sandbox, []byte, error) {
	return nil, nil, nil
}

func (stubPoolProvider) Update(context.Context, sandbox.SandboxRef, []byte, sandbox.UpdateOptions) (*sandbox.Sandbox, []byte, error) {
	return nil, nil, nil
}

func (stubPoolProvider) Start(context.Context, sandbox.SandboxRef, []byte) ([]byte, error) {
	return nil, nil
}

func (stubPoolProvider) Stop(context.Context, sandbox.SandboxRef, []byte, time.Duration) ([]byte, error) {
	return nil, nil
}

func (stubPoolProvider) Restart(context.Context, sandbox.SandboxRef, []byte, time.Duration) ([]byte, error) {
	return nil, nil
}

func (stubPoolProvider) Archive(context.Context, sandbox.SandboxRef, []byte) ([]byte, error) {
	return nil, nil
}

func (stubPoolProvider) Remove(context.Context, sandbox.SandboxRef, []byte) ([]byte, error) {
	return nil, nil
}

func (stubPoolProvider) Get(context.Context, sandbox.SandboxRef, []byte) (*sandbox.Sandbox, error) {
	return nil, nil
}

func (stubPoolProvider) AcquireHTTPClient(context.Context, sandbox.SandboxRef, []byte, []string) (*transport.HTTPClientLease, error) {
	return nil, nil
}

type startingPoolProvider struct {
	stubPoolProvider
	observe func(context.Context, sandbox.PoolManager, *model.Pool, []string, func(context.Context) error) error
}

func (p startingPoolProvider) ReconcilePool(ctx context.Context, manager sandbox.PoolManager, _ *model.Project, _ *model.SandboxProviderInstance, pool *model.Pool, images []string, begin func(context.Context) error) error {
	return p.observe(ctx, manager, pool, images, begin)
}

func TestReplacementClosesPlacementBeforePreload(t *testing.T) {
	previous := defaultSandboxImage()
	t.Cleanup(func() { setDefaultSandboxImage(previous) })
	setDefaultSandboxImage("ghcr.io/discobox-ai/discobox-sandbox-agent:v1")
	ctx := context.Background()
	appStore, _ := newPoolReconcilerTestStore(t)
	manager := sandbox.NewProviderManager()
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "startup", Name: "startup"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	pool := &model.Pool{ID: "pool-1", ProjectID: provider.ProjectID,
		PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: provider.ID},
		Ready:        true, Schedulable: true, RegisteredAt: &now, LastSeenAt: &now, StatusReportedAt: &now,
	}
	pool.SetState(model.PoolStateActive)
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	sb := &model.Sandbox{ProjectID: pool.ProjectID, PoolID: pool.ID}
	manager.RegisterProvider("startup", startingPoolProvider{observe: func(ctx context.Context, _ sandbox.PoolManager, _ *model.Pool, images []string, begin func(context.Context) error) error {
		if len(images) == 0 {
			t.Fatal("startup received no project images")
		}
		if _, err := appStore.SchedulablePoolForSandbox(ctx, sb); err != nil {
			t.Fatalf("live pool initially unavailable: %v", err)
		}
		if err := begin(ctx); err != nil {
			return err
		}
		// An old ready heartbeat can arrive while the image load is in flight.
		if _, err := appStore.UpdatePoolStatus(ctx, pool.ID, true, true, false, 0, 0, 0, nil); err != nil {
			return err
		}
		if _, err := appStore.SchedulablePoolForSandbox(ctx, sb); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("preloading pool admitted work: %v", err)
		}
		return nil
	}})
	r := NewPoolReconciler(appStore, manager, NewControlPlane(appStore, nil))
	if _, err := r.Reconcile(ctx, PoolDirtyID(pool.ProjectID, pool.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := appStore.SchedulablePoolForSandbox(ctx, sb); err != nil {
		t.Fatalf("completed startup still gated: %v", err)
	}
}

func TestStartupReconcileDoesNotRenewPreviousOfflineFailure(t *testing.T) {
	ctx := context.Background()
	st, _ := newPoolReconcilerTestStore(t)
	manager := sandbox.NewProviderManager()
	manager.RegisterProvider("stub", stubPoolProvider{})
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "stub", Name: "stub"}
	if err := st.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1", Ready: true, Schedulable: true,
		RegisteredAt: &old, LastSeenAt: &old, StatusReportedAt: &old, ReconciledAt: &old,
		PoolManifest:      model.PoolManifest{Name: "pool-1", ProviderInstanceID: provider.ID},
		ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.DesiredStatePresent}}
	pool.RecordFailure(model.PoolStateOffline, "pool agent has not reported", "")
	if err := st.CreatePool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginPoolHealthChecks(ctx); err != nil {
		t.Fatal(err)
	}
	rec := NewPoolReconciler(st, manager, NewControlPlane(st, nil))
	if _, err := rec.Reconcile(ctx, PoolDirtyID(pool.ProjectID, pool.ID)); err != nil {
		t.Fatal(err)
	}
	updated, err := st.GetPoolByID(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ErrorMessage != nil || updated.Health(time.Now()) != model.PoolHealthUnknown {
		t.Fatalf("startup renewed the old offline failure: %+v", updated)
	}
	if updated.ReconciledAt == nil || updated.ReconciledAt.Before(*updated.HealthCheckStartedAt) {
		t.Fatal("runtime result lacks a current observation timestamp")
	}
	// An actual provider failure on this run must still end the wait promptly.
	manager.RegisterProvider("stub", failingPoolProvider{stubPoolProvider{}})
	if _, err := rec.Reconcile(ctx, PoolDirtyID(pool.ProjectID, pool.ID)); err == nil {
		t.Fatal("expected runtime failure")
	}
	updated, err = st.GetPoolByID(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ErrorMessage == nil || updated.ReconciledAt == nil || updated.ReconciledAt.Before(*updated.HealthCheckStartedAt) {
		t.Fatal("current runtime failure was not timestamped")
	}
}
