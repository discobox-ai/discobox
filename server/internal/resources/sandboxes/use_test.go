package sandboxes

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/server/internal/transport"
	"gorm.io/gorm"
)

// containerProvider is a pool that holds a sandbox's tree and may or may not
// hold its container. A create gives it one, a teardown takes it away, and
// everything that needs one answers ErrNoContainer without it — which is how
// the pool agent answers for the sandbox the incident behind ADR 26-10-12-711
// left behind.
type containerProvider struct {
	recordingProvider
	container atomic.Bool
	// createErr fails every create, so a rebuild never lands.
	createErr atomic.Pointer[error]
	// archived is the pool's marker between a repair's teardown and the
	// create that clears it: a start in that window is told the sandbox is
	// archived. createGate, when set, holds every create until it is closed,
	// which keeps that window open.
	archived   atomic.Bool
	createGate atomic.Pointer[chan struct{}]
	// ensureErr answers every ensure, in place of looking for the container.
	ensureErr atomic.Pointer[error]

	creates, archives, ensures, starts atomic.Int32
}

func (p *containerProvider) Create(context.Context, sandbox.SandboxRef, []byte, sandbox.CreateOptions) (*sandbox.Sandbox, []byte, error) {
	p.creates.Add(1)
	if gate := p.createGate.Load(); gate != nil {
		<-*gate
	}
	if err := p.createErr.Load(); err != nil {
		return nil, nil, *err
	}
	p.archived.Store(false)
	p.container.Store(true)
	return nil, nil, nil
}

func (p *containerProvider) Archive(context.Context, sandbox.SandboxRef, []byte) ([]byte, error) {
	p.archives.Add(1)
	p.container.Store(false)
	p.archived.Store(true)
	return nil, nil
}

func (p *containerProvider) EnsureRunning(context.Context, sandbox.SandboxRef, []byte) error {
	p.ensures.Add(1)
	if err := p.ensureErr.Load(); err != nil {
		return *err
	}
	// As the pool agent does: a tree marked archived answers that, not that
	// it has no container, until a create clears the marker.
	if p.archived.Load() {
		return sandbox.ErrArchived
	}
	if !p.container.Load() {
		return sandbox.ErrNoContainer
	}
	return nil
}

func (p *containerProvider) Start(_ context.Context, _ sandbox.SandboxRef, state []byte) ([]byte, error) {
	p.starts.Add(1)
	if p.archived.Load() {
		return state, sandbox.ErrArchived
	}
	if !p.container.Load() {
		return state, sandbox.ErrNoContainer
	}
	return state, nil
}

func (p *containerProvider) Stop(_ context.Context, _ sandbox.SandboxRef, state []byte, _ time.Duration) ([]byte, error) {
	if !p.container.Load() {
		return state, sandbox.ErrNoContainer
	}
	return state, nil
}

func (p *containerProvider) AcquireHTTPClient(context.Context, sandbox.SandboxRef, []byte, []string, sandbox.Use) (*transport.HTTPClientLease, error) {
	return transport.NewHTTPClientLease(http.DefaultClient, func() {}), nil
}

const createFailure = `do request: Post "http://127.0.0.1:32892/sandboxes": EOF`

// useFixture is a sandbox on a ready pool that has no container there, with
// the reconcile engine running, so an intent or a dirty mark recorded by a use
// of the sandbox is converged the way it is in the server. The sandbox starts
// out failed and settled on that create failure.
func useFixture(t *testing.T) (*Service, *containerProvider, *gorm.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	appStore := store.New(db.Write, db.Read)
	if err := db.Write.WithContext(ctx).Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := appStore.CreateSandboxProviderInstance(ctx, &model.SandboxProviderInstance{
		ID: "prov-1", ProjectID: "project-1", Type: "test", Name: "test",
	}); err != nil {
		t.Fatalf("create provider instance: %v", err)
	}
	reportedAt := time.Now().Add(-time.Second)
	if err := appStore.CreatePool(ctx, &model.Pool{
		StatusReportedAt: &reportedAt,
		ID:               "pool-1", ProjectID: "project-1", Ready: true,
		PoolManifest:      model.PoolManifest{Name: "pool-1", ProviderInstanceID: "prov-1"},
		ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.DesiredStatePresent, State: model.PoolStateActive},
	}); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	failure := createFailure
	if err := appStore.CreateSandbox(ctx, &model.Sandbox{
		ID: "sb-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "sb-1",
		SandboxManifest: model.SandboxManifest{HarnessMode: "run", Bundle: codexBundle, BundleDigest: codexBundleV1},
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState: model.DesiredStatePresent, State: model.SandboxStateFailed,
			Generation: 3, ObservedGeneration: 3, ErrorMessage: &failure,
		},
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	provider := &containerProvider{}
	manager := sandbox.NewProviderManager(appStore)
	manager.RegisterProvider("test", provider)
	manager.SetDefault("test")

	engine, err := reconcile.New(db.Write, reconcile.Options{SingleNode: true})
	if err != nil {
		t.Fatalf("create reconcile engine: %v", err)
	}
	service := NewService(appStore, manager, "user-1", engine)
	if err := service.RegisterJobs(); err != nil {
		t.Fatalf("register sandbox reconciler: %v", err)
	}
	engineCtx, stop := context.WithCancel(ctx)
	if err := engine.Start(engineCtx); err != nil {
		stop()
		t.Fatalf("start reconcile engine: %v", err)
	}
	t.Cleanup(func() {
		stop()
		_ = engine.Stop(context.Background())
	})
	return service, provider, db.Write
}

func storedSandbox(t *testing.T, service *Service) *model.Sandbox {
	t.Helper()
	sb, err := service.store.GetSandbox(context.Background(), "project-1", "sb-1")
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	return sb
}

func useContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// The incident: a create failed after the old container was removed, the
// failure settled, and nothing short of a typed repair brought the discobox
// back. Using it now repairs it — the full repair, teardown first — and the
// row keeps what was repaired and why, because landing clears the failure.
func TestAnActionRepairsAFailedSandboxWithNoContainer(t *testing.T) {
	service, provider, _ := useFixture(t)
	ctx := useContext(t)

	lease, sb, err := service.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", nil, services.SandboxAction("exec attach"))
	if err != nil {
		t.Fatalf("acquire for an action on a failed sandbox with no container: %v", err)
	}
	if lease == nil {
		t.Fatal("no lease onto the repaired sandbox")
	}
	lease.Release()

	if sb.State != model.SandboxStateReady || sb.ErrorMessage != nil || !sb.Converged() {
		t.Fatalf("sandbox = state %s, error %v, generation %d/%d; want it ready and settled",
			sb.State, sb.ErrorMessage, sb.Generation, sb.ObservedGeneration)
	}
	if sb.Generation != 4 || sb.RepairGeneration != 4 {
		t.Fatalf("generation = %d, repair generation = %d; want one new intent, and it a repair", sb.Generation, sb.RepairGeneration)
	}
	repair := sb.AutomaticRepair
	if repair == nil || repair.Generation != 4 || repair.Trigger != "exec attach" || repair.Failure != createFailure {
		t.Fatalf("automatic repair record = %+v, want generation 4, triggered by the attach, answering %q", repair, createFailure)
	}
	if provider.archives.Load() != 1 || provider.creates.Load() != 1 {
		t.Fatalf("teardowns = %d, creates = %d; want the repair's one of each", provider.archives.Load(), provider.creates.Load())
	}
	if !provider.container.Load() {
		t.Fatal("the lease was handed out for a sandbox that still has no container")
	}
}

// One. A repair that fails leaves the discobox on the repair's own generation,
// and no later use tries again: it is refused at once, with the failure the
// repair answered, the repair's own, and the command that repairs it.
//
// The repair tore the runtime down before its create failed, so the pool now
// calls the sandbox archived rather than without a container. That is still
// this refusal, not "it is still archived on its pool".
func TestAFailedAutomaticRepairIsNotRepeated(t *testing.T) {
	service, provider, _ := useFixture(t)
	ctx := useContext(t)
	rebuildFailure := errors.New("invalid mount config: bind source path does not exist")
	provider.createErr.Store(&rebuildFailure)

	for attempt := 1; attempt <= 3; attempt++ {
		started := time.Now()
		_, _, err := service.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", nil, services.SandboxAction("http"))
		var statusErr interface{ StatusCode() int }
		if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusConflict {
			t.Fatalf("use %d: err = %v, want a 409", attempt, err)
		}
		for _, want := range []string{createFailure, rebuildFailure.Error(), "discobox admin box repair sb-1"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("use %d: refusal %q does not say %q", attempt, err, want)
			}
		}
		if attempt > 1 && time.Since(started) > sandboxReachablePollInterval {
			t.Fatalf("use %d waited %s for a repair that is not coming", attempt, time.Since(started))
		}
	}
	sb := storedSandbox(t, service)
	if sb.Generation != 4 || provider.creates.Load() != 1 {
		t.Fatalf("generation = %d, creates = %d; want the one automatic repair and nothing after it", sb.Generation, provider.creates.Load())
	}
	if sb.AutomaticRepairAllowed() {
		t.Fatal("a sandbox whose automatic repair failed still allows another")
	}

	// Explicit intent moves the generation on, and with it the bound.
	if _, err := service.recordSandboxIntent(ctx, "project-1", "sb-1", model.DesiredStatePresent); err != nil {
		t.Fatalf("record intent: %v", err)
	}
	waitFor(t, func() bool { return storedSandbox(t, service).Converged() })
	if sb := storedSandbox(t, service); !sb.AutomaticRepairAllowed() {
		t.Fatalf("after an explicit intent failed too (generation %d, automatic repair %+v), one more automatic repair is not allowed",
			sb.Generation, sb.AutomaticRepair)
	}
}

// Concurrent uses of the same failed discobox collapse to one repair: the
// condition is re-read in the transaction that records the intent, and the
// losers wait for the winner's generation.
func TestConcurrentActionsCollapseToOneRepair(t *testing.T) {
	service, provider, _ := useFixture(t)
	ctx := useContext(t)

	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, _, err := service.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", nil, services.SandboxAction("exec create"))
			if lease != nil {
				lease.Release()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent use of the sandbox failed: %v", err)
		}
	}
	if sb := storedSandbox(t, service); sb.Generation != 4 || provider.archives.Load() != 1 {
		t.Fatalf("generation = %d, teardowns = %d; want one repair for six uses", sb.Generation, provider.archives.Load())
	}
}

// `discobox start` after the one automatic repair failed is told the same
// thing an action is, whichever way the pool describes what the repair left.
func TestAStartAfterAFailedAutomaticRepairIsRefusedWithBothFailures(t *testing.T) {
	service, provider, _ := useFixture(t)
	ctx := useContext(t)
	rebuildFailure := errors.New("sandbox image is not available")
	provider.createErr.Store(&rebuildFailure)

	for attempt := 1; attempt <= 2; attempt++ {
		_, err := service.StartSandbox(ctx, "project-1", "sb-1", services.StartSandboxBody{})
		if err == nil || !strings.Contains(err.Error(), createFailure) || !strings.Contains(err.Error(), rebuildFailure.Error()) ||
			!strings.Contains(err.Error(), "discobox admin box repair sb-1") {
			t.Fatalf("start %d: err = %v, want both failures and the repair command", attempt, err)
		}
	}
	if sb := storedSandbox(t, service); sb.Generation != 4 || provider.creates.Load() != 1 {
		t.Fatalf("generation = %d, creates = %d; want the one repair", sb.Generation, provider.creates.Load())
	}
}

// A read never repairs, and neither does an observe: there is no reason to
// rebuild a discobox to answer one. They answer at once, as they always did,
// and what the pool then says about the missing container is theirs to pass on.
func TestReadsAndObservesNeverRepair(t *testing.T) {
	service, provider, _ := useFixture(t)
	ctx := useContext(t)

	for _, use := range []services.SandboxUse{services.SandboxRead, services.SandboxObserve} {
		lease, _, err := service.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", nil, use)
		if err != nil {
			t.Fatalf("%s acquire: %v", use.Kind, err)
		}
		lease.Release()
	}
	sb := storedSandbox(t, service)
	if sb.Generation != 3 || sb.AutomaticRepair != nil || sb.State != model.SandboxStateFailed {
		t.Fatalf("after a read and an observe: generation %d, automatic repair %+v, state %s; want the sandbox untouched",
			sb.Generation, sb.AutomaticRepair, sb.State)
	}
	if provider.ensures.Load() != 0 || provider.creates.Load() != 0 {
		t.Fatalf("ensures = %d, creates = %d; want the pool asked for nothing", provider.ensures.Load(), provider.creates.Load())
	}
}

// A failed discobox that kept its container is not `no container`: it is
// started and used as it was before, and nothing is rebuilt.
func TestAFailedSandboxThatKeptItsContainerIsNotRepaired(t *testing.T) {
	service, provider, _ := useFixture(t)
	ctx := useContext(t)
	provider.container.Store(true)

	lease, _, err := service.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", nil, services.SandboxAction("ssh"))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	lease.Release()
	if sb := storedSandbox(t, service); sb.Generation != 3 || sb.AutomaticRepair != nil {
		t.Fatalf("generation = %d, automatic repair = %+v; want no intent recorded", sb.Generation, sb.AutomaticRepair)
	}
	if provider.archives.Load() != 0 || provider.creates.Load() != 0 {
		t.Fatal("a sandbox that still had its container was rebuilt")
	}
}

// A discobox that lost its container without failing is rebuilt by the
// ordinary reconcile, which a use asks for now rather than leaving to the next
// complete sync. That is a dirty mark and not intent: no generation moves, no
// teardown runs, and it is not a repair.
func TestALostContainerIsRebuiltOnUseWithoutARepair(t *testing.T) {
	service, provider, db := useFixture(t)
	ctx := useContext(t)
	if err := db.Model(&model.Sandbox{}).Where("id = ?", "sb-1").
		Updates(map[string]any{"state": model.SandboxStateReady, "error_message": nil}).Error; err != nil {
		t.Fatalf("make the sandbox ready: %v", err)
	}

	lease, sb, err := service.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", nil, services.SandboxAction("exec input"))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	lease.Release()
	if sb.Generation != 3 || sb.AutomaticRepair != nil || sb.RepairGeneration != 0 {
		t.Fatalf("generation = %d, repair generation = %d, automatic repair = %+v; want a rebuild with no intent",
			sb.Generation, sb.RepairGeneration, sb.AutomaticRepair)
	}
	if provider.archives.Load() != 0 || provider.creates.Load() == 0 || !provider.container.Load() {
		t.Fatalf("teardowns = %d, creates = %d, container = %v; want it created and nothing torn down",
			provider.archives.Load(), provider.creates.Load(), provider.container.Load())
	}
}

// The repair re-pins the way a typed one does (ADR 0064), except where the
// project holds its sandboxes on the bundles they have: nobody asked for this
// repair, so under a `manual` policy it rebuilds on the pin it found.
func TestAutomaticRepairRepinsOnlyUnderAnAutomaticPolicy(t *testing.T) {
	for policy, wantDigest := range map[string]string{
		"":                                  codexBundleV2,
		model.SandboxUpgradePolicyAutomatic: codexBundleV2,
		model.SandboxUpgradePolicyManual:    codexBundleV1,
	} {
		t.Run("policy="+policy, func(t *testing.T) {
			service, _, db := useFixture(t)
			ctx := useContext(t)
			config := bundledConfig(t, service.store, codexBundle, codexBundleV2)
			if err := db.Model(&model.Sandbox{}).Where("id = ?", "sb-1").Update("harness_config_id", config.ID).Error; err != nil {
				t.Fatalf("give the sandbox its harness config: %v", err)
			}
			if err := db.Model(&model.Project{}).Where("id = ?", "project-1").Update("sandbox_upgrade_policy", policy).Error; err != nil {
				t.Fatalf("set the upgrade policy: %v", err)
			}

			lease, sb, err := service.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", nil, services.SandboxAction("git"))
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			lease.Release()
			if sb.AutomaticRepair == nil || sb.BundleDigest != wantDigest {
				t.Fatalf("automatic repair = %+v, digest = %s; want a repair on %s", sb.AutomaticRepair, sb.BundleDigest, wantDigest)
			}
		})
	}
}

// `discobox start` on the incident's discobox answered that it had no
// container. A start is a use of it, so it repairs it once and is then sent
// again. A stop is not, and is refused as before.
func TestAStartRepairsAndAStopDoesNot(t *testing.T) {
	service, provider, _ := useFixture(t)
	ctx := useContext(t)

	if _, err := service.StopSandbox(ctx, "project-1", "sb-1", services.StopSandboxBody{}); !errors.Is(err, ErrNoContainer) {
		t.Fatalf("stop of a sandbox with no container: err = %v, want the no-container refusal", err)
	}
	if sb := storedSandbox(t, service); sb.Generation != 3 {
		t.Fatalf("generation = %d after a stop, want it unchanged", sb.Generation)
	}

	if _, err := service.StartSandbox(ctx, "project-1", "sb-1", services.StartSandboxBody{}); err != nil {
		t.Fatalf("start of a failed sandbox with no container: %v", err)
	}
	sb := storedSandbox(t, service)
	if sb.AutomaticRepair == nil || sb.AutomaticRepair.Trigger != "start" || sb.State != model.SandboxStateReady {
		t.Fatalf("automatic repair = %+v, state = %s; want a repair triggered by the start", sb.AutomaticRepair, sb.State)
	}
	// Refused once for want of a container, and sent again once it had one.
	if provider.starts.Load() != 2 {
		t.Fatalf("start instructions = %d, want the refused one and the one after the repair", provider.starts.Load())
	}
}

// A repair tears the runtime down before it rebuilds it, and between the two
// the pool calls the sandbox archived. A start waiting on its own repair is
// asked again through that window, and must read the answer as the rebuild it
// is rather than fail the command with "still archived on its pool".
func TestAStartWaitsThroughItsRepairsTeardown(t *testing.T) {
	service, provider, _ := useFixture(t)
	ctx := useContext(t)
	gate := make(chan struct{})
	provider.createGate.Store(&gate)

	done := make(chan error, 1)
	go func() {
		_, err := service.StartSandbox(ctx, "project-1", "sb-1", services.StartSandboxBody{})
		done <- err
	}()
	// Torn down and held before the create: every start sent now is told the
	// sandbox is archived.
	waitFor(t, func() bool { return provider.archived.Load() && provider.starts.Load() >= 3 })
	select {
	case err := <-done:
		t.Fatalf("the start answered %v in the middle of the repair it caused", err)
	default:
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("start: %v", err)
	}
	if sb := storedSandbox(t, service); sb.State != model.SandboxStateReady || sb.AutomaticRepair == nil || sb.AutomaticRepair.LandedAt == nil {
		t.Fatalf("state = %s, automatic repair = %+v; want the repair landed", sb.State, sb.AutomaticRepair)
	}
}

// The record outlives a repair that worked, and a sandbox can fail again on
// the same generation: its container is lost later, and the rebuild fails.
// That failure is not the repair's own. It gets a repair of its own, and is
// not refused as "the one automatic repair failed too" over a failure the
// first repair fixed.
func TestAFailureAfterALandedRepairIsRepairedAgain(t *testing.T) {
	service, provider, db := useFixture(t)
	ctx := useContext(t)

	lease, sb, err := service.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", nil, services.SandboxAction("exec attach"))
	if err != nil {
		t.Fatalf("first use: %v", err)
	}
	lease.Release()
	if sb.AutomaticRepair == nil || sb.AutomaticRepair.LandedAt == nil || sb.AutomaticRepairSpent() {
		t.Fatalf("automatic repair = %+v; want it landed, and no longer what bounds the next", sb.AutomaticRepair)
	}

	// The container is lost and its rebuild fails, on the repair's generation.
	provider.container.Store(false)
	later := "create failed: no space left on device"
	if err := db.Model(&model.Sandbox{}).Where("id = ?", "sb-1").
		Updates(map[string]any{"state": model.SandboxStateFailed, "error_message": later}).Error; err != nil {
		t.Fatalf("fail the sandbox again: %v", err)
	}

	lease, sb, err = service.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", nil, services.SandboxAction("ssh"))
	if err != nil {
		t.Fatalf("use after the second failure: %v", err)
	}
	lease.Release()
	repair := sb.AutomaticRepair
	if sb.Generation != 5 || repair == nil || repair.Generation != 5 || repair.Trigger != "ssh" || repair.Failure != later {
		t.Fatalf("generation = %d, automatic repair = %+v; want a second repair, answering %q", sb.Generation, repair, later)
	}
}

// A pool that holds nothing of a settled sandbox has answered: nothing is
// coming that would change it, so the action is told at once and does not sit
// out the stall budget.
func TestAnActionIsAnsweredAtOnceWhenThePoolHoldsNothing(t *testing.T) {
	service, provider, db := useFixture(t)
	ctx := useContext(t)
	if err := db.Model(&model.Sandbox{}).Where("id = ?", "sb-1").
		Updates(map[string]any{"state": model.SandboxStateReady, "error_message": nil}).Error; err != nil {
		t.Fatalf("make the sandbox ready: %v", err)
	}
	notFound := sandbox.ErrNotFound
	provider.ensureErr.Store(&notFound)

	started := time.Now()
	_, _, err := service.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", nil, services.SandboxAction("http"))
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusConflict || !strings.Contains(err.Error(), "repair it") {
		t.Fatalf("err = %v, want a 409 that names repair", err)
	}
	if waited := time.Since(started); waited > sandboxReachablePollInterval {
		t.Fatalf("the action waited %s on a pool that had answered", waited)
	}
}

// A start is a command that wants an answer: it waits only for a rebuild it
// found coming. A discobox still being created is refused at once, as before.
func TestAStartDoesNotWaitForASandboxBeingCreated(t *testing.T) {
	service, _, db := useFixture(t)
	ctx := useContext(t)
	if err := db.Model(&model.Sandbox{}).Where("id = ?", "sb-1").
		Updates(map[string]any{"state": model.SandboxStatePending, "error_message": nil}).Error; err != nil {
		t.Fatalf("make the sandbox pending: %v", err)
	}
	started := time.Now()
	if _, err := service.StartSandbox(ctx, "project-1", "sb-1", services.StartSandboxBody{}); !errors.Is(err, ErrNoContainer) {
		t.Fatalf("start of a pending sandbox with no container: err = %v, want the refusal", err)
	}
	if waited := time.Since(started); waited > sandboxReachablePollInterval {
		t.Fatalf("the start waited %s", waited)
	}
	if sb := storedSandbox(t, service); sb.AutomaticRepair != nil {
		t.Fatalf("a sandbox that never failed was repaired: %+v", sb.AutomaticRepair)
	}
}

// The HTTP port proxy acquires on every request. A sandbox the row reads as
// running is not asked about: the pool agent starts it again by the lease's
// mark if a stop landed after this look.
func TestAnActionOnARunningSandboxDoesNotAskThePool(t *testing.T) {
	service, provider, db := useFixture(t)
	ctx := useContext(t)
	if err := db.Model(&model.Sandbox{}).Where("id = ?", "sb-1").
		Updates(map[string]any{"state": model.SandboxStateReady, "error_message": nil, "runtime_state": model.SandboxRuntimeStateRunning}).Error; err != nil {
		t.Fatalf("make the sandbox running: %v", err)
	}

	lease, _, err := service.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", nil, services.SandboxAction("http"))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	lease.Release()
	if provider.ensures.Load() != 0 {
		t.Fatalf("ensures = %d, want none for a sandbox that reads as running", provider.ensures.Load())
	}
}

// The row's power state is an observation, up to a complete sync old. A failed
// sandbox last seen running is still asked about, or the stale `running` would
// hand out a lease onto a container that is gone and leave the repair to
// whoever tried again a minute later.
func TestAFailedSandboxLastSeenRunningIsStillAsked(t *testing.T) {
	service, provider, db := useFixture(t)
	ctx := useContext(t)
	if err := db.Model(&model.Sandbox{}).Where("id = ?", "sb-1").
		Update("runtime_state", model.SandboxRuntimeStateRunning).Error; err != nil {
		t.Fatalf("record a stale running observation: %v", err)
	}

	lease, sb, err := service.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", nil, services.SandboxAction("http"))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	lease.Release()
	if sb.AutomaticRepair == nil || !provider.container.Load() {
		t.Fatalf("automatic repair = %+v, container = %v; want the sandbox repaired", sb.AutomaticRepair, provider.container.Load())
	}
}

// A pool agent from before the ensure answers that it has no such operation.
// It still starts a stopped sandbox itself when the request reaches it, so the
// action goes ahead, and nothing is repaired on its word.
func TestAnActionGoesAheadOnAPoolAgentWithoutTheEnsure(t *testing.T) {
	service, provider, _ := useFixture(t)
	ctx := useContext(t)
	unsupported := sandbox.ErrPoolAgentUnsupported
	provider.ensureErr.Store(&unsupported)

	lease, _, err := service.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", nil, services.SandboxAction("exec create"))
	if err != nil {
		t.Fatalf("acquire on an older pool agent: %v", err)
	}
	lease.Release()
	if sb := storedSandbox(t, service); sb.Generation != 3 {
		t.Fatalf("generation = %d, want no intent recorded for an agent that cannot say there is no container", sb.Generation)
	}
}

// A sandbox archived by intent is not repaired by being used. An action is
// refused before its pool is asked, with what undoes the archive — not told to
// wait for an unarchive nobody asked for, or to repair what repair refuses.
func TestAnActionOnAnArchivedSandboxIsRefused(t *testing.T) {
	service, provider, db := useFixture(t)
	ctx := useContext(t)
	if err := db.Model(&model.Sandbox{}).Where("id = ?", "sb-1").
		Updates(map[string]any{"desired_state": model.DesiredStateArchived, "state": model.SandboxStateArchived, "error_message": nil}).Error; err != nil {
		t.Fatalf("archive the sandbox: %v", err)
	}

	_, _, err := service.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", nil, services.SandboxAction("exec attach"))
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusConflict || !strings.Contains(err.Error(), "unarchive it first") {
		t.Fatalf("err = %v, want a 409 saying to unarchive", err)
	}
	if sb := storedSandbox(t, service); sb.Generation != 3 || provider.ensures.Load() != 0 || provider.creates.Load() != 0 {
		t.Fatalf("generation = %d, ensures = %d, creates = %d; want the pool asked for nothing",
			sb.Generation, provider.ensures.Load(), provider.creates.Load())
	}
}

func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
