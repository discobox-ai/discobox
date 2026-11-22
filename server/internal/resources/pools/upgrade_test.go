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
	manager    *sandbox.ProviderManager
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
	controlPlane := NewControlPlane(appStore, manager, nil)
	watch := controlPlane.Upgrades()
	clock := time.Now()
	watch.now = func() time.Time {
		clock = clock.Add(upgradeCheckInterval)
		return clock
	}
	return &upgradeFixture{
		t: t, store: appStore, db: db, watch: watch, manager: manager,
		reconciler: NewPoolReconciler(appStore, manager, controlPlane, nil),
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
// older agent. A watched pool that is deleted, or whose provider's type this
// server cannot build, drops out. One on a disabled provider is still waited
// on: its pools keep working, upgrades included.
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
	if got := f.pending(); len(got) != 1 || got["kept"] == "" {
		t.Fatalf("pending = %v, want the kept pool still waited on with its provider disabled", got)
	}
	if err := f.db.Model(&model.SandboxProviderInstance{}).Where("id = ?", "provider-1").Update("type", "unsupported").Error; err != nil {
		t.Fatal(err)
	}
	if got := f.pending(); len(got) != 0 {
		t.Fatalf("pending = %v, want none on a provider this server cannot build", got)
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

// agentStartingPoolProvider starts the pool's agent on the passes start names, as a
// provider does that makes or starts the pool's box or restarts its agent;
// a pass that comes up names unreachable answers ErrPoolNotReachable after it
// started the agent, as a VM pool's machine does while it boots.
type agentStartingPoolProvider struct {
	stubPoolProvider
	passes      *int
	start       map[int]sandbox.AgentStartKind
	unreachable map[int]bool
}

func (p agentStartingPoolProvider) ReconcilePool(ctx context.Context, _ sandbox.PoolManager, _ *model.Project, _ *model.SandboxProviderInstance, _ *model.Pool, _ func(context.Context) error) error {
	*p.passes++
	if kind := p.start[*p.passes]; kind != 0 {
		sandbox.ReportAgentStarted(ctx, time.Now(), kind)
	}
	if p.unreachable[*p.passes] {
		return sandbox.ErrPoolNotReachable
	}
	return nil
}

func (f *upgradeFixture) useProvider(name string, provider sandbox.Provider) {
	f.t.Helper()
	f.manager.RegisterProvider(name, provider)
	if err := f.db.Model(&model.SandboxProviderInstance{}).Where("id = ?", "provider-1").Update("type", name).Error; err != nil {
		f.t.Fatal(err)
	}
}

// A pass that starts the pool's agent is often not the one that settles: a
// VM pool's machine is started, does not answer while it boots, and the pass
// that settles starts nothing. The new agent's first report, sent at boot
// before that settle, is the new agent's, so the pool is upgraded without
// waiting for its next heartbeat (#1194).
func TestUpgradeWatchCountsTheReportOfAnAgentStartedBeforeTheSettle(t *testing.T) {
	f := newUpgradeFixture(t)
	f.addPool("pool-1", true)
	f.useProvider("starts", agentStartingPoolProvider{passes: new(int), start: map[int]sandbox.AgentStartKind{1: sandbox.AgentStartBox}, unreachable: map[int]bool{1: true}})
	f.start()
	f.reconcile("pool-1")
	f.report("pool-1", true)
	if got := f.pending()["pool-1"]; got != waitingForReconcile {
		t.Fatalf("started, not yet settled: %q", got)
	}
	f.reconcile("pool-1")
	if got := f.pending(); len(got) != 0 {
		t.Fatalf("the started agent's boot report did not count: %v", got)
	}
}

// A report from before the latest start is the agent's that start replaced:
// a booted machine whose moved bundles then restart its agent waits for the
// restarted agent's report.
func TestUpgradeWatchCountsOnlyReportsAfterTheLatestStart(t *testing.T) {
	f := newUpgradeFixture(t)
	f.addPool("pool-1", true)
	f.useProvider("starts", agentStartingPoolProvider{passes: new(int), start: map[int]sandbox.AgentStartKind{1: sandbox.AgentStartBox, 2: sandbox.AgentStartBox}, unreachable: map[int]bool{1: true}})
	f.start()
	f.reconcile("pool-1")
	f.report("pool-1", true)
	f.reconcile("pool-1")
	if got := f.pending()["pool-1"]; got != "waiting for its agent to report" {
		t.Fatalf("restarted agent, earlier agent's report only: %q", got)
	}
	f.report("pool-1", true)
	if got := f.pending(); len(got) != 0 {
		t.Fatalf("restarted agent's report did not count: %v", got)
	}
}

// firstReport is the pool's agent sending its first report, ready, now, as
// the agent service records it: stored, then told to the watch.
func (f *upgradeFixture) firstReport(id string) {
	f.t.Helper()
	f.report(id, true)
	pool, err := f.store.GetPoolByID(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	f.watch.firstReported(id, *pool.StatusReportedAt)
}

// An agent restarted in a running box sends its first report before the
// restart returns, so before the pass settles. Recorded from before the
// restart, that marked report counts, while the old agent's heartbeat in the
// same window does not (ADR 26-11-15-602 §3, #1203).
func TestUpgradeWatchCountsTheFirstReportOfAnAgentRestartedInPlace(t *testing.T) {
	f := newUpgradeFixture(t)
	f.addPool("pool-1", true)
	starts := map[int]sandbox.AgentStartKind{1: sandbox.AgentStartInPlace}
	f.useProvider("restarts", agentStartingPoolProvider{passes: new(int), start: starts, unreachable: map[int]bool{1: true}})
	f.start()
	f.reconcile("pool-1")
	// The old agent's heartbeat, sent after the start was recorded but
	// before the restart stopped it.
	f.report("pool-1", true)
	f.reconcile("pool-1")
	if got := f.pending()["pool-1"]; got != "waiting for its agent to report" {
		t.Fatalf("the old agent's heartbeat after the in-place start counted: %q", got)
	}

	g := newUpgradeFixture(t)
	g.addPool("pool-1", true)
	g.useProvider("restarts", agentStartingPoolProvider{passes: new(int), start: starts, unreachable: map[int]bool{1: true}})
	g.start()
	g.reconcile("pool-1")
	g.firstReport("pool-1")
	g.reconcile("pool-1")
	if got := g.pending(); len(got) != 0 {
		t.Fatalf("the restarted agent's first report, before the settle, did not count: %v", got)
	}
}

// A first report from before the in-place start is the old agent's, sent
// when it started; it never counts. A restart that restarted nothing sends no
// first report, and the pool waits for a report from the settle on.
func TestUpgradeWatchFallsBackToTheSettleAfterAnInPlaceStart(t *testing.T) {
	f := newUpgradeFixture(t)
	f.addPool("pool-1", true)
	f.useProvider("restarts", agentStartingPoolProvider{passes: new(int), start: map[int]sandbox.AgentStartKind{1: sandbox.AgentStartInPlace}})
	f.start()
	f.firstReport("pool-1")
	f.reconcile("pool-1")
	if got := f.pending()["pool-1"]; got != "waiting for its agent to report" {
		t.Fatalf("a first report from before the in-place start counted: %q", got)
	}
	f.report("pool-1", true)
	if got := f.pending(); len(got) != 0 {
		t.Fatalf("a report after the settle did not count: %v", got)
	}
}

// A box this run started and a later pass then restarts in place, whose
// booted agent is from before the first-report mark and reported unmarked:
// the restarted agent's marked first report counts, as it does for an agent
// that was running before the server started (#1203).
func TestUpgradeWatchCountsTheFirstReportOfAnAgentRestartedAfterABoot(t *testing.T) {
	f := newUpgradeFixture(t)
	f.addPool("pool-1", true)
	f.useProvider("restarts", agentStartingPoolProvider{
		passes:      new(int),
		start:       map[int]sandbox.AgentStartKind{1: sandbox.AgentStartBox, 2: sandbox.AgentStartInPlace},
		unreachable: map[int]bool{1: true, 2: true},
	})
	f.start()
	f.reconcile("pool-1")
	// The booted agent, from the previous release, reports unmarked.
	f.report("pool-1", true)
	f.reconcile("pool-1")
	if got := f.pending()["pool-1"]; got != waitingForReconcile {
		t.Fatalf("restarting, not yet settled: %q", got)
	}
	f.firstReport("pool-1")
	f.reconcile("pool-1")
	if got := f.pending(); len(got) != 0 {
		t.Fatalf("the restarted agent's first report did not count: %v", got)
	}
}
