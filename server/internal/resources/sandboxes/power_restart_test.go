package sandboxes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/resources/pools"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// bootingPoolProvider is a pool whose agent a server restart takes away for
// a while: every pool's VM boots again after a server start, and until it is
// back a start cannot be delivered (#893). Its starts are fenced as the pool
// agent fences them, by calling the server's own AuthorizePoolSandboxStart
// under its lock before executing. A start it loses is executed later, as one
// whose answer was lost is. The engine calls it from its own goroutines, so
// everything it records is under its lock.
type bootingPoolProvider struct {
	recordingProvider
	mu        sync.Mutex
	authorize func(ctx context.Context, sandboxID string, epoch int64) error
	wake      sandbox.Wake
	reachable bool
	// lose has the agent take a start and lose its answer: queued in lost,
	// answered as the pool not reachable.
	lose     bool
	lost     []sandbox.SandboxRef
	refused  int
	unfenced int
	fenced   int
	started  []time.Time
}

func (p *bootingPoolProvider) PowerDown(_ context.Context, _ sandbox.SandboxRef, state []byte, _ sandbox.PowerDownReason) (sandbox.Wake, []byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.wake, state, nil
}

func (p *bootingPoolProvider) Start(ctx context.Context, ref sandbox.SandboxRef, _ []byte) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.reachable {
		p.refused++
		return nil, fmt.Errorf("pool pool-t: %w: agent socket not bound yet", sandbox.ErrPoolNotReachable)
	}
	if p.lose {
		p.lost = append(p.lost, ref)
		return nil, fmt.Errorf("pool pool-t: %w: start sandbox on pool-t: EOF", sandbox.ErrPoolNotReachable)
	}
	return nil, p.execute(ctx, ref)
}

// execute is the agent running a start under its power lock: a fenced start
// is authorized with the server first, as the pool agent does.
func (p *bootingPoolProvider) execute(ctx context.Context, ref sandbox.SandboxRef) error {
	if ref.StartEpoch == nil {
		p.unfenced++
		return errors.New("an unfenced start reached the pool")
	}
	if err := p.authorize(ctx, ref.SandboxID, *ref.StartEpoch); err != nil {
		p.fenced++
		return fmt.Errorf("%w: %w", sandbox.ErrStartNotAuthorized, err)
	}
	p.started = append(p.started, time.Now())
	return nil
}

// replayLost runs the starts whose answers were lost, as the agent that took
// them does.
func (p *bootingPoolProvider) replayLost(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	lost := p.lost
	p.lost = nil
	p.lose = false
	for _, ref := range lost {
		_ = p.execute(ctx, ref)
	}
}

func (p *bootingPoolProvider) setLose(lose bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lose = lose
}

func (p *bootingPoolProvider) counts() (lost, fenced, unfenced int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.lost), p.fenced, p.unfenced
}

func (p *bootingPoolProvider) setReachable(reachable bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reachable = reachable
}

func (p *bootingPoolProvider) starts() (started []time.Time, refused int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]time.Time(nil), p.started...), p.refused
}

// restartFixture is one database that server processes come and go over:
// each is an engine with the power reconcile registered and a pools service
// that records its pool's reports, and nothing in memory survives from one to
// the next.
type restartFixture struct {
	t        *testing.T
	db       *database.DB
	st       *store.Store
	manager  *sandbox.ProviderManager
	provider *bootingPoolProvider
	sandbox  string
	id       string
	seq      int64
}

type restartServer struct {
	engine    *reconcile.Engine
	pools     *pools.Service
	sandboxes *Service
}

func newRestartFixture(t *testing.T) *restartFixture {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Write.WithContext(ctx).Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatal(err)
	}
	st := store.New(db.Write, db.Read)
	provider := &bootingPoolProvider{reachable: true}
	manager := sandbox.NewProviderManager(st)
	manager.RegisterProvider("test", provider)
	manager.SetDefault("test")
	if err := st.CreateSandboxProviderInstance(ctx, &model.SandboxProviderInstance{ID: "prov-t", ProjectID: "project-1", Type: "test", Name: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePool(ctx, &model.Pool{ID: "pool-t", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool-t", ProviderInstanceID: "prov-t"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Write.Model(&model.Pool{}).Where("id = ?", "pool-t").Update("state", model.PoolStateActive).Error; err != nil {
		t.Fatal(err)
	}
	sb := &model.Sandbox{ProjectID: "project-1", PoolID: "pool-t", CreatedByUserID: "user-1", Name: "sleepy",
		SandboxManifest: model.SandboxManifest{Image: "image"}}
	sb.DesiredState = model.DesiredStatePresent
	if err := st.CreateSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	return &restartFixture{t: t, db: db, st: st, manager: manager, provider: provider, sandbox: sb.ID, id: SandboxDirtyID("project-1", sb.ID)}
}

// start is a server process starting: pool health is checked afresh, as
// BeginPoolHealthChecks does at every server start, so no pool reads ready
// until it reports again.
func (f *restartFixture) start() *restartServer {
	f.t.Helper()
	ctx := context.Background()
	if err := f.st.BeginPoolHealthChecks(ctx); err != nil {
		f.t.Fatal(err)
	}
	engine, err := reconcile.New(f.db.Write, reconcile.Options{
		SingleNode: true, PollInterval: 10 * time.Millisecond,
		BackoffBase: 20 * time.Millisecond, BackoffMax: 100 * time.Millisecond,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := engine.Register(SandboxPowerResourceType, &sandboxPowerReconciler{store: f.st, providers: f.manager, now: time.Now}); err != nil {
		f.t.Fatal(err)
	}
	if err := engine.Start(ctx); err != nil {
		f.t.Fatal(err)
	}
	svc := NewService(f.st, nil, "user-1", engine)
	svc.sandboxProviders = f.manager
	server := &restartServer{engine: engine, pools: pools.NewService(f.st, f.manager, nil, svc), sandboxes: svc}
	// The pool's agent authorizes fenced starts with the server it is
	// connected to: this one, from now on.
	f.provider.mu.Lock()
	f.provider.authorize = func(ctx context.Context, sandboxID string, epoch int64) error {
		poolCtx := auth.WithPrincipal(ctx, auth.Principal{Type: auth.PrincipalTypePool, PoolID: "pool-t"})
		return server.pools.AuthorizePoolSandboxStart(poolCtx, "pool-t", sandboxID, epoch)
	}
	f.provider.mu.Unlock()
	return server
}

func (f *restartFixture) stop(server *restartServer) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.engine.Stop(ctx); err != nil {
		f.t.Fatal(err)
	}
}

// poolReports is the pool's agent sending its heartbeat to a server.
func (f *restartFixture) poolReports(server *restartServer, ready bool) {
	f.t.Helper()
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypePool, PoolID: "pool-t"})
	if _, err := server.pools.UpdatePoolStatus(ctx, "pool-t", services.UpdatePoolStatusBody{Ready: ready, Schedulable: ready}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *restartFixture) setRuntime(state string) {
	f.t.Helper()
	f.seq++
	if _, err := f.st.ApplySandboxStateReports(context.Background(), store.SandboxStateReportBatch{
		PoolID: "pool-t", BootID: "boot-1", Sequence: f.seq, ReportedAt: time.Now(),
		Reports: []store.SandboxStateReport{{SandboxID: f.sandbox, State: state}},
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *restartFixture) wakeAt() *time.Time {
	f.t.Helper()
	current, err := f.st.GetSandbox(context.Background(), "project-1", f.sandbox)
	if err != nil {
		f.t.Fatal(err)
	}
	return current.WakeAt
}

func (f *restartFixture) waitFor(what string, done func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sleepWithWake has the sandbox sleep on a server, with a wake at wakeAt, and
// that server stop: what a box running `discobox-power sleep --wake-in` sees
// before a server restart.
func (f *restartFixture) sleepWithWake(wakeAt time.Time) {
	f.t.Helper()
	ctx := context.Background()
	f.provider.wake = sandbox.Wake{At: wakeAt, Name: "ci"}
	f.setRuntime(model.SandboxRuntimeStateRunning)
	status, err := json.Marshal(map[string]any{"autostop": map[string]any{"sleep": map[string]any{"request": "sleep"}}})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.st.UpdateSandboxAgentStatus(ctx, "project-1", f.sandbox, status, time.Now(), nil); err != nil {
		f.t.Fatal(err)
	}
	before := f.start()
	if err := before.engine.MarkDirtyDrift(ctx, SandboxPowerResourceType, f.id); err != nil {
		f.t.Fatal(err)
	}
	f.waitFor("the power-down to record the wake", func() bool { return f.wakeAt() != nil })
	f.stop(before)
	f.setRuntime(model.SandboxRuntimeStateStopped)
}

// A wake that fell due while the server was down is not sent to a pool that
// has not reported ready to the new server, and is sent as soon as it has
// (#1192, ADR 26-11-15-829 §2): the ready report marks it, well inside the
// minute the wake would otherwise wait.
func TestAWakeDueWhileTheServerWasDownFiresWhenItsPoolIsReady(t *testing.T) {
	f := newRestartFixture(t)
	wakeAt := time.Now().Add(500 * time.Millisecond).UTC()
	f.sleepWithWake(wakeAt)
	time.Sleep(time.Until(wakeAt) + 50*time.Millisecond)

	after := f.start()
	t.Cleanup(func() { f.stop(after) })
	f.poolReports(after, false)
	time.Sleep(300 * time.Millisecond)
	if started, refused := f.provider.starts(); len(started) != 0 || refused != 0 {
		t.Fatalf("starts %d, refused %d before the pool was ready; want nothing sent", len(started), refused)
	}
	if f.wakeAt() == nil {
		t.Fatal("the wake was dropped while its pool was not ready")
	}

	readyAt := time.Now()
	f.poolReports(after, true)
	f.waitFor("the wake to start the sandbox", func() bool { started, _ := f.provider.starts(); return len(started) > 0 })
	started, _ := f.provider.starts()
	if started[0].Sub(readyAt) > 2*time.Second {
		t.Fatalf("started %v after the pool's ready report", started[0].Sub(readyAt))
	}
	if got := f.wakeAt(); got != nil {
		t.Fatalf("wake = %v after its start, want it spent", got)
	}
	time.Sleep(200 * time.Millisecond)
	if started, _ := f.provider.starts(); len(started) != 1 {
		t.Fatalf("starts = %d, want exactly one", len(started))
	}
}

// A wake still ahead when the server restarts is sent at its time.
func TestAWakeAheadOfARestartFiresAtItsTime(t *testing.T) {
	f := newRestartFixture(t)
	wakeAt := time.Now().Add(time.Second).UTC()
	f.sleepWithWake(wakeAt)

	after := f.start()
	t.Cleanup(func() { f.stop(after) })
	f.poolReports(after, true)
	f.waitFor("the wake to start the sandbox", func() bool { started, _ := f.provider.starts(); return len(started) > 0 })
	started, _ := f.provider.starts()
	if started[0].Before(wakeAt) {
		t.Fatalf("started at %v, before its wake at %v", started[0], wakeAt)
	}
	if late := started[0].Sub(wakeAt); late > time.Second {
		t.Fatalf("started %v after its wake", late)
	}
}

// A pool that reports ready while its agent cannot yet be reached — about to
// be replaced, or its socket not bound — fails the start, and the wake is kept
// and sent again under backoff until the pool takes it (§1).
func TestAWakeIsKeptUntilItsPoolTakesTheStart(t *testing.T) {
	f := newRestartFixture(t)
	wakeAt := time.Now().Add(300 * time.Millisecond).UTC()
	f.sleepWithWake(wakeAt)
	f.provider.setReachable(false)

	after := f.start()
	t.Cleanup(func() { f.stop(after) })
	f.poolReports(after, true)
	f.waitFor("the wake to try the unreachable pool", func() bool { _, refused := f.provider.starts(); return refused > 1 })
	if f.wakeAt() == nil {
		t.Fatal("the wake was spent on starts the pool never took")
	}
	f.provider.setReachable(true)
	f.waitFor("the wake to start the sandbox", func() bool { started, _ := f.provider.starts(); return len(started) > 0 })
	if got := f.wakeAt(); got != nil {
		t.Fatalf("wake = %v after its start, want it spent", got)
	}
}

// A person's stop admitted while the wake waits for its pool wins: it cancels
// the wake, and the epoch it moves refuses at the pool any start fenced on
// the old one, so no start runs after the stop (§3).
func TestAStopWhileTheWakeWaitsForItsPoolWins(t *testing.T) {
	f := newRestartFixture(t)
	wakeAt := time.Now().Add(300 * time.Millisecond).UTC()
	f.sleepWithWake(wakeAt)
	f.provider.setReachable(false)

	after := f.start()
	t.Cleanup(func() { f.stop(after) })
	f.poolReports(after, true)
	f.waitFor("a first attempt the pool did not take", func() bool { _, refused := f.provider.starts(); return refused > 0 })
	svc := NewService(f.st, nil, "user-1", after.engine)
	if _, err := svc.cancelSandboxWake(context.Background(), "project-1", f.sandbox); err != nil {
		t.Fatal(err)
	}
	f.provider.setReachable(true)
	time.Sleep(500 * time.Millisecond)
	if started, _ := f.provider.starts(); len(started) != 0 {
		t.Fatalf("starts = %d after a stop overrode the wake, want none", len(started))
	}
	if got := f.wakeAt(); got != nil {
		t.Fatalf("wake = %v after a stop, want it canceled", got)
	}
}

// A start whose answer was lost, replayed by the pool's agent after a
// person's stop, is refused at the pool: the stop moved the epoch the start
// was fenced on, and the server's authorization refuses it (§3). The wake the
// stop canceled is not sent again.
func TestALostWakeStartReplayedAfterAStopIsRefusedAtThePool(t *testing.T) {
	f := newRestartFixture(t)
	wakeAt := time.Now().Add(300 * time.Millisecond).UTC()
	f.sleepWithWake(wakeAt)
	f.provider.setLose(true)

	after := f.start()
	t.Cleanup(func() { f.stop(after) })
	f.poolReports(after, true)
	f.waitFor("a start whose answer is lost", func() bool { lost, _, _ := f.provider.counts(); return lost > 0 })
	if f.wakeAt() == nil {
		t.Fatal("the wake was spent on a start whose answer was lost")
	}
	// A person stops the sandbox; then the lost start runs at the pool.
	svc := NewService(f.st, nil, "user-1", after.engine)
	if _, err := svc.cancelSandboxWake(context.Background(), "project-1", f.sandbox); err != nil {
		t.Fatal(err)
	}
	f.provider.replayLost(context.Background())
	time.Sleep(300 * time.Millisecond)
	started, _ := f.provider.starts()
	_, fenced, unfenced := f.provider.counts()
	if len(started) != 0 || unfenced != 0 {
		t.Fatalf("starts %d (unfenced %d) after a person's stop, want none", len(started), unfenced)
	}
	if fenced == 0 {
		t.Fatal("the replayed start was not refused by the pool's fence")
	}
	if got := f.wakeAt(); got != nil {
		t.Fatalf("wake = %v after a stop, want it canceled", got)
	}
}

// A pool down long enough to read `offline` is made `active` again by its
// reconcile after the report that earned it, so no report sees it turn able
// to take wakes. The reconcile's settle hands the sandbox service the pool as
// it found it and as it leaves it, in the transaction that writes it, and that
// is what sends the wake (§2) — well inside the minute it would otherwise wait.
func TestAWakeFiresWhenItsPoolIsBroughtBackFromOffline(t *testing.T) {
	f := newRestartFixture(t)
	wakeAt := time.Now().Add(300 * time.Millisecond).UTC()
	f.sleepWithWake(wakeAt)
	if err := f.db.Write.Model(&model.Pool{}).Where("id = ?", "pool-t").Update("state", model.PoolStateOffline).Error; err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(wakeAt) + 50*time.Millisecond)

	after := f.start()
	t.Cleanup(func() { f.stop(after) })
	// Its first report finds it offline, and leaves it offline.
	f.poolReports(after, true)
	time.Sleep(200 * time.Millisecond)
	if started, refused := f.provider.starts(); len(started) != 0 || refused != 0 {
		t.Fatalf("starts %d, refused %d while the pool read offline; want nothing sent", len(started), refused)
	}
	// The pool reconcile brings it back, as PoolReconciler.settle does.
	activeAt := time.Now()
	if err := f.st.Transaction(context.Background(), func(txStore *store.Store, txDB *gorm.DB) error {
		found, err := txStore.GetPoolByID(context.Background(), "pool-t")
		if err != nil {
			return err
		}
		if err := txDB.Model(&model.Pool{}).Where("id = ?", "pool-t").Update("state", model.PoolStateActive).Error; err != nil {
			return err
		}
		left, err := txStore.GetPoolByID(context.Background(), "pool-t")
		if err != nil {
			return err
		}
		return after.sandboxes.MarkPoolDueWakesTx(context.Background(), txStore, txDB, found, left)
	}); err != nil {
		t.Fatal(err)
	}
	f.waitFor("the wake to start the sandbox", func() bool { started, _ := f.provider.starts(); return len(started) > 0 })
	started, _ := f.provider.starts()
	if started[0].Sub(activeAt) > 2*time.Second {
		t.Fatalf("started %v after the pool was brought back", started[0].Sub(activeAt))
	}
}
