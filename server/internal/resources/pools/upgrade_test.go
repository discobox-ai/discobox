package pools

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// upgradeFixture is a store with one stub provider and the pools a test adds,
// and a watch whose clock moves past its answer cache on every read.
type upgradeFixture struct {
	t          *testing.T
	store      *store.Store
	db         *gorm.DB
	reconciler *PoolReconciler
	watch      *UpgradeWatch
}

func newUpgradeFixture(t *testing.T) *upgradeFixture {
	t.Helper()
	appStore, db := newPoolReconcilerTestStore(t)
	manager := sandbox.NewProviderManager(appStore)
	manager.RegisterProvider("stub", stubPoolProvider{})
	manager.RegisterProvider("keeps", keepingPoolProvider{})
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "stub", Name: "stub"}
	if err := appStore.CreateSandboxProviderInstance(context.Background(), provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	controlPlane := NewControlPlane(appStore, nil)
	watch := controlPlane.Upgrades()
	clock := time.Now()
	watch.now = func() time.Time {
		clock = clock.Add(upgradeCheckInterval)
		return clock
	}
	return &upgradeFixture{
		t: t, store: appStore, db: db, watch: watch,
		reconciler: NewPoolReconciler(appStore, manager, controlPlane),
	}
}

// addPool creates a pool, registered (one an earlier server ran) or not.
func (f *upgradeFixture) addPool(id string, registered bool) {
	f.t.Helper()
	pool := &model.Pool{
		ID:           id,
		ProjectID:    "project-1",
		PoolManifest: model.PoolManifest{Name: id, ProviderInstanceID: "provider-1"},
	}
	if registered {
		earlier := time.Now().Add(-time.Hour).UTC()
		pool.RegisteredAt, pool.LastSeenAt, pool.StatusReportedAt = &earlier, &earlier, &earlier
		pool.Ready, pool.Schedulable = true, true
	}
	if err := f.store.CreatePool(context.Background(), pool); err != nil {
		f.t.Fatalf("create pool: %v", err)
	}
}

// start is a server start: health checks begin, then the watch fixes its pools.
func (f *upgradeFixture) start() {
	f.t.Helper()
	ctx := context.Background()
	if err := f.store.BeginPoolHealthChecks(ctx); err != nil {
		f.t.Fatal(err)
	}
	if err := f.watch.Begin(ctx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *upgradeFixture) reconcile(id string) {
	f.t.Helper()
	if _, err := f.reconciler.Reconcile(context.Background(), PoolDirtyID("project-1", id)); err != nil {
		f.t.Fatalf("reconcile %s: %v", id, err)
	}
}

// report is the pool's agent reporting, ready or not, now.
func (f *upgradeFixture) report(id string, ready bool) {
	f.t.Helper()
	now := time.Now().UTC().Add(time.Millisecond)
	if err := f.db.Model(&model.Pool{}).Where("id = ?", id).
		Updates(map[string]any{"status_reported_at": now, "last_seen_at": now, "ready": ready}).Error; err != nil {
		f.t.Fatal(err)
	}
}

func (f *upgradeFixture) pending() map[string]string {
	f.t.Helper()
	pending := f.watch.Pending(context.Background())
	waiting := map[string]string{}
	for _, pool := range pending {
		waiting[pool.ID] = pool.WaitingOn
	}
	return waiting
}

// A pool an earlier server ran is upgrading until, in this run, it has
// reconciled and its agent has reported ready since; a report from before the
// reconcile — the old agent's — does not count.
func TestUpgradeWatchWaitsForAReconcileAndAReportAfterIt(t *testing.T) {
	f := newUpgradeFixture(t)
	f.addPool("pool-1", true)
	f.start()
	f.report("pool-1", true)

	if got := f.pending()["pool-1"]; got != "waiting for its reconcile" {
		t.Fatalf("before its reconcile: %q", got)
	}
	f.reconcile("pool-1")
	if got := f.pending()["pool-1"]; got != "waiting for its agent to report" {
		t.Fatalf("reconciled, old agent's report only: %q", got)
	}
	f.report("pool-1", false)
	if got := f.pending()["pool-1"]; got != "its agent reports "+model.PoolHealthNotReady {
		t.Fatalf("agent not ready: %q", got)
	}
	f.report("pool-1", true)
	if got := f.pending(); len(got) != 0 {
		t.Fatalf("upgraded pool still pending: %v", got)
	}
}

// A failed reconcile in this run is named with its reason, and the pool
// waits until a later one succeeds.
func TestUpgradeWatchNamesAFailedReconcile(t *testing.T) {
	f := newUpgradeFixture(t)
	f.addPool("pool-1", true)
	f.start()
	now := time.Now().UTC().Add(time.Millisecond)
	message := "pool agent did not answer\nsecond line"
	if err := f.db.Model(&model.Pool{}).Where("id = ?", "pool-1").
		Updates(map[string]any{"reconciled_at": now, "error_message": message}).Error; err != nil {
		t.Fatal(err)
	}
	if got := f.pending()["pool-1"]; got != "its reconcile failed: pool agent did not answer" {
		t.Fatalf("failed reconcile: %q", got)
	}
	f.reconcile("pool-1")
	f.report("pool-1", true)
	if got := f.pending(); len(got) != 0 {
		t.Fatalf("recovered pool still pending: %v", got)
	}
}

// Only pools registered before the start are waited on: a new pool has no
// older agent. A watched pool that is deleted, or whose provider is disabled,
// drops out.
func TestUpgradeWatchWaitsOnlyOnPoolsItCanUpgrade(t *testing.T) {
	f := newUpgradeFixture(t)
	f.addPool("new", false)
	f.addPool("deleted", true)
	f.addPool("kept", true)
	f.start()
	if got := f.pending(); len(got) != 2 || got["new"] != "" {
		t.Fatalf("pending = %v, want the two registered pools", got)
	}
	if err := f.db.Model(&model.Pool{}).Where("id = ?", "deleted").Update("desired_state", model.DesiredStateDeleted).Error; err != nil {
		t.Fatal(err)
	}
	if got := f.pending(); len(got) != 1 || got["kept"] == "" {
		t.Fatalf("pending = %v, want only the kept pool", got)
	}
	if err := f.db.Model(&model.SandboxProviderInstance{}).Where("id = ?", "provider-1").Update("disabled", true).Error; err != nil {
		t.Fatal(err)
	}
	if got := f.pending(); len(got) != 0 {
		t.Fatalf("pending = %v, want none on a disabled provider", got)
	}
}

// Once clear, the watch stays clear: a pool going down later is that pool's
// health, not an upgrade still in progress.
func TestUpgradeWatchStaysClear(t *testing.T) {
	f := newUpgradeFixture(t)
	f.addPool("pool-1", true)
	f.start()
	f.reconcile("pool-1")
	f.report("pool-1", true)
	if got := f.pending(); len(got) != 0 {
		t.Fatalf("pending = %v", got)
	}
	f.report("pool-1", false)
	if got := f.pending(); len(got) != 0 {
		t.Fatalf("cleared watch reports %v again", got)
	}
}

// A read within the cache interval answers the last answer without asking
// the store again.
func TestUpgradeWatchReusesItsAnswerWithinAnInterval(t *testing.T) {
	f := newUpgradeFixture(t)
	f.addPool("pool-1", true)
	f.start()
	fixed := time.Now()
	f.watch.now = func() time.Time { return fixed }
	if first := f.watch.Pending(context.Background()); len(first) != 1 {
		t.Fatalf("first = %v", first)
	}
	f.reconcile("pool-1")
	f.report("pool-1", true)
	if again := f.watch.Pending(context.Background()); len(again) != 1 {
		t.Fatalf("cached answer = %v", again)
	}
}

// Before its first pass the watch answers every watched pool: a probe that
// cannot wait for the pass, or a pass that fails, never reports the server
// settled.
func TestUpgradeWatchStartsWithEveryWatchedPoolPending(t *testing.T) {
	f := newUpgradeFixture(t)
	f.addPool("pool-1", true)
	f.addPool("pool-2", true)
	f.start()
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	pending := f.watch.Pending(gone)
	if len(pending) != 2 || pending[0].WaitingOn != waitingForReconcile {
		t.Fatalf("pending before the first pass = %+v", pending)
	}
}

// keepingPoolProvider converges by keeping the pool's box as it was, as a
// provider does when the release it would place does not resolve or apply.
type keepingPoolProvider struct{ stubPoolProvider }

func (keepingPoolProvider) ReconcilePool(ctx context.Context, _ sandbox.PoolManager, _ *model.Project, _ *model.SandboxProviderInstance, _ *model.Pool, _ func(context.Context) error) error {
	sandbox.ReportBoxKept(ctx, errors.New("resolve the pool bundle: registry unreachable"))
	return nil
}

// A provider that keeps the pool's previous box settles cleanly, and its old
// agent goes on reporting ready; the pool is still upgrading until a reconcile
// places the new box.
func TestUpgradeWatchWaitsOnAPoolWhoseProviderKeptItsBox(t *testing.T) {
	f := newUpgradeFixture(t)
	f.addPool("pool-1", true)
	if err := f.db.Model(&model.SandboxProviderInstance{}).Where("id = ?", "provider-1").Update("type", "keeps").Error; err != nil {
		t.Fatal(err)
	}
	f.start()
	f.reconcile("pool-1")
	f.report("pool-1", true)
	if got := f.pending()["pool-1"]; got != "keeping its previous agent: resolve the pool bundle: registry unreachable" {
		t.Fatalf("kept box: %q", got)
	}
	if err := f.db.Model(&model.SandboxProviderInstance{}).Where("id = ?", "provider-1").Update("type", "stub").Error; err != nil {
		t.Fatal(err)
	}
	f.reconcile("pool-1")
	f.report("pool-1", true)
	if got := f.pending(); len(got) != 0 {
		t.Fatalf("placed box still pending: %v", got)
	}
}
