package sandboxes

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

// provisioningProvider answers acquires the way the pool-backed provider does
// for a sandbox that has not been dispatched yet: ErrNotFound until the runtime
// state names a pool, a lease once it does.
type provisioningProvider struct {
	recordingProvider
	ready atomic.Bool
}

func (p *provisioningProvider) AcquireHTTPClient(context.Context, sandbox.SandboxRef, []byte, []string) (*transport.HTTPClientLease, error) {
	if !p.ready.Load() {
		return nil, sandbox.ErrNotFound
	}
	return transport.NewHTTPClientLease(http.DefaultClient, func() {}), nil
}

// attachWaitFixture is a sandbox on a ready pool whose provider is still
// provisioning it.
func attachWaitFixture(t *testing.T) (*Service, *provisioningProvider) {
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
	pool := &model.Pool{
		ID: "pool-1", ProjectID: "project-1", Ready: true,
		PoolManifest:      model.PoolManifest{Name: "pool-1", ProviderInstanceID: "prov-1"},
		ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.DesiredStatePresent, State: model.PoolStateActive},
	}
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	if err := appStore.CreateSandbox(ctx, &model.Sandbox{
		ID: "sb-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "sb-1",
		ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.DesiredStatePresent, State: model.SandboxStatePending},
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	provider := &provisioningProvider{}
	manager := sandbox.NewProviderManager()
	manager.RegisterProvider("test", provider)
	manager.SetDefault("test")

	service := NewService(appStore, manager, "user-1", nil)
	return service, provider
}

// The launcher creates a sandbox and attaches to it immediately, which is the
// whole point of ADR 0039: the attach lands while the sandbox is still being
// provisioned and waits for it instead of being told "sandbox not found".
func TestAwaitSandboxHTTPClientWaitsForProvisioning(t *testing.T) {
	service, provider := attachWaitFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, _, err := service.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", nil); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("acquire on a provisioning sandbox = %v, want %v", err, sandbox.ErrNotFound)
	}

	type result struct {
		lease *transport.HTTPClientLease
		err   error
	}
	done := make(chan result, 1)
	go func() {
		lease, _, err := service.AwaitSandboxHTTPClient(ctx, "project-1", "sb-1", nil)
		done <- result{lease: lease, err: err}
	}()

	select {
	case got := <-done:
		t.Fatalf("await returned before the sandbox was reachable: lease=%v err=%v", got.lease, got.err)
	case <-time.After(100 * time.Millisecond):
	}

	// Provisioning finishes, and the next pass sees it.
	provider.ready.Store(true)
	sb, err := service.store.GetSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	sb.SetState(model.SandboxStateReady)
	if err := service.store.UpdateSandbox(ctx, sb); err != nil {
		t.Fatalf("update sandbox: %v", err)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("await after provisioning: %v", got.err)
		}
		if got.lease == nil {
			t.Fatal("await returned no lease")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("await did not wake once the sandbox was reachable")
	}
}

// A push-delivered sandbox is reachable long before it is usable: it has a
// container — and so a runtime state naming its pool — from the moment it parks
// waiting for the client's push. Handing an attach through then would start it
// against an empty workspace, so the wait covers delivery too: `awaiting_source`
// while the push is outstanding, and an unobserved generation while the
// reconciler materializes what was pushed.
func TestAwaitSandboxHTTPClientWaitsForSourceDelivery(t *testing.T) {
	service, provider := attachWaitFixture(t)
	provider.ready.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	park := func(state string, generation, observed int64) {
		t.Helper()
		sb, err := service.store.GetSandbox(ctx, "project-1", "sb-1")
		if err != nil {
			t.Fatalf("get sandbox: %v", err)
		}
		sb.SetState(state)
		sb.Generation, sb.ObservedGeneration = generation, observed
		if err := service.store.UpdateSandbox(ctx, sb); err != nil {
			t.Fatalf("update sandbox: %v", err)
		}
	}

	// Parked for the push. The plain acquire is happy — that is the hazard.
	park(model.SandboxStateAwaitingSource, 1, 1)
	if _, _, err := service.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", nil); err != nil {
		t.Fatalf("acquire on a parked sandbox = %v, want success (this is what the wait exists to cover)", err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := service.AwaitSandboxHTTPClient(ctx, "project-1", "sb-1", nil)
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("await returned while the source was still outstanding: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// complete-source-push records the intent: the generation moves and the
	// reconciler has not acted on it yet.
	park(model.SandboxStateAwaitingSource, 2, 1)
	select {
	case err := <-done:
		t.Fatalf("await returned while the pushed source was still being materialized: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// The reconciler materializes the source and converges.
	park(model.SandboxStateReady, 2, 2)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("await after the source landed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("await did not wake once the sandbox was usable")
	}
}

// The stall budget only holds a wait open while something is actually moving,
// so the mark has to tell progress from liveness. Both rows it reads are
// rewritten on a timer by things that are neither — the pool agent's status
// heartbeat, and the complete state sync restamping every sandbox it hosts —
// and a mark that moved for those could never expire.
func TestProvisioningMarkIgnoresLiveness(t *testing.T) {
	service, _ := attachWaitFixture(t)
	ctx := context.Background()

	mark := func() provisioningMark {
		t.Helper()
		sb, err := service.store.GetSandbox(ctx, "project-1", "sb-1")
		if err != nil {
			t.Fatalf("get sandbox: %v", err)
		}
		return service.provisioningMark(ctx, sb)
	}
	base := mark()

	// The pool agent's status heartbeat: a whole-row save every 30 seconds,
	// reporting the same capacity and the same readiness as last time.
	if _, err := service.store.UpdatePoolStatus(ctx, "pool-1", true, true, false, 4, 1<<30, 1<<30, nil); err != nil {
		t.Fatalf("pool status: %v", err)
	}
	if got := mark(); got != base {
		t.Fatalf("a pool heartbeat moved the mark:\n got %+v\nwant %+v", got, base)
	}

	// The complete sync re-reports every sandbox on the pool on its interval,
	// restamping the report watermark whether or not the state moved.
	for seq := int64(1); seq <= 2; seq++ {
		if _, err := service.store.ApplySandboxStateReports(ctx, store.SandboxStateReportBatch{
			PoolID: "pool-1", BootID: "boot-1", Sequence: seq, ReportedAt: time.Now().UTC(), Complete: true,
			Reports: []store.SandboxStateReport{{SandboxID: "sb-1", State: model.SandboxRuntimeStateStopped}},
		}); err != nil {
			t.Fatalf("state report: %v", err)
		}
	}
	stopped := mark()
	if stopped == base {
		t.Fatal("the runtime state moving did not move the mark")
	}
	// The second identical report is the restamp, and must not count.
	if _, err := service.store.ApplySandboxStateReports(ctx, store.SandboxStateReportBatch{
		PoolID: "pool-1", BootID: "boot-1", Sequence: 3, ReportedAt: time.Now().UTC(), Complete: true,
		Reports: []store.SandboxStateReport{{SandboxID: "sb-1", State: model.SandboxRuntimeStateStopped}},
	}); err != nil {
		t.Fatalf("state report: %v", err)
	}
	if got := mark(); got != stopped {
		t.Fatalf("a repeated state report moved the mark:\n got %+v\nwant %+v", got, stopped)
	}
}

// What the mark does have to see is progress, wherever it happens: an image
// pulling into the sandbox, and the pool host itself coming up. The pool half
// matters most while a sandbox is waiting on a pool that is not ready, because
// then the sandbox row does not change at all.
func TestProvisioningMarkSeesProgress(t *testing.T) {
	service, _ := attachWaitFixture(t)
	ctx := context.Background()

	mark := func() provisioningMark {
		t.Helper()
		sb, err := service.store.GetSandbox(ctx, "project-1", "sb-1")
		if err != nil {
			t.Fatalf("get sandbox: %v", err)
		}
		return service.provisioningMark(ctx, sb)
	}

	previous := mark()
	for _, step := range []struct {
		name string
		do   func() error
	}{
		{"sandbox pull progress", func() error {
			return service.store.ApplySandboxProgressReports(ctx, "pool-1", time.Now().UTC(),
				[]store.SandboxProgressReport{{SandboxID: "sb-1", Progress: []byte(`{"phase":"pull"}`)}})
		}},
		{"pool provisioning progress", func() error {
			return service.store.RecordPoolProvisionProgress(ctx, "pool-1", []byte(`{"phase":"boot"}`), time.Now().UTC())
		}},
		{"pool image staging", func() error {
			return service.store.RecordPoolImageStage(ctx, "pool-1", []byte(`{"phase":"pull"}`), true, time.Now().UTC())
		}},
	} {
		if err := step.do(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		got := mark()
		if got == previous {
			t.Fatalf("%s did not move the mark: %+v", step.name, got)
		}
		previous = got
	}
}

// Another sandbox in the project is not this wait's business: the mark is built
// from this sandbox's own row and the pool hosting it, so a busy project cannot
// hold a stalled sandbox open.
func TestProvisioningMarkIgnoresOtherSandboxes(t *testing.T) {
	service, _ := attachWaitFixture(t)
	ctx := context.Background()

	sb, err := service.store.GetSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	base := service.provisioningMark(ctx, sb)

	other := &model.Sandbox{
		ID: "sb-2", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "sb-2",
		ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.DesiredStatePresent, State: model.SandboxStatePending},
	}
	if err := service.store.CreateSandbox(ctx, other); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	other.SetState(model.SandboxStateReady)
	if err := service.store.UpdateSandbox(ctx, other); err != nil {
		t.Fatalf("update sandbox: %v", err)
	}
	if got := service.provisioningMark(ctx, sb); got != base {
		t.Fatalf("another sandbox moved the mark:\n got %+v\nwant %+v", got, base)
	}
}

// Waiting is for a sandbox that is on its way. A sandbox on its way out
// produces the same "not yet" refusal and never resolves, so it is answered
// rather than waited on — as is a failure that needs new intent, and an error
// that says something other than "not yet".
func TestSandboxCanBecomeReachableAnswersTerminalConditions(t *testing.T) {
	present := func(state string) *model.Sandbox {
		return &model.Sandbox{ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.DesiredStatePresent, State: state}}
	}
	if !sandboxCanBecomeReachable(sandbox.ErrNotFound, present(model.SandboxStatePending)) {
		t.Fatal("a pending sandbox is not waited for")
	}
	if !sandboxCanBecomeReachable(ErrSandboxPoolNotReachable, present(model.SandboxStateReady)) {
		t.Fatal("a pool that is not up yet is not waited for")
	}
	if sandboxCanBecomeReachable(errors.New("provider is not configured"), present(model.SandboxStatePending)) {
		t.Fatal("an error that is not 'not yet' is waited on")
	}
	if sandboxCanBecomeReachable(sandbox.ErrNotFound, nil) {
		t.Fatal("a sandbox with no row is waited on")
	}
	for _, state := range []string{model.SandboxStateFailed, model.SandboxStateArchived, model.SandboxStateDeleted} {
		if sandboxCanBecomeReachable(sandbox.ErrNotFound, present(state)) {
			t.Fatalf("a %s sandbox is waited on", state)
		}
	}
	archived := present(model.SandboxStateReady)
	archived.DesiredState = model.DesiredStateArchived
	if sandboxCanBecomeReachable(sandbox.ErrNotFound, archived) {
		t.Fatal("a sandbox being archived is waited on")
	}
}

// A client that goes away takes its wait with it, rather than holding a
// request open until the stall budget runs out.
func TestAwaitSandboxHTTPClientEndsWithItsCaller(t *testing.T) {
	service, _ := attachWaitFixture(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, _, err := service.AwaitSandboxHTTPClient(ctx, "project-1", "sb-1", nil)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("await after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("await outlived its caller")
	}
}
