package sandboxes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/sourcecache"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// powerProvider records power-downs and starts, and answers a power-down with
// what the test sets.
type powerProvider struct {
	recordingProvider
	powerDowns []sandbox.PowerDownReason
	starts     int
	suspends   int
	suspendErr error
	// startErr is what Start answers with; a start that errs is not counted.
	startErr error
	// startEpochs records the epoch each start was fenced on, nil unfenced.
	startEpochs []*int64
	// blockStart holds a start until its context ends, as one in flight
	// when the server stops is.
	blockStart bool
	// duringStart runs inside a start, as something landing while it is in
	// flight would.
	duringStart func()
	wake        sandbox.Wake
	refuse      bool
	// during runs inside the power-down, as an outside instruction landing
	// while it is in flight would.
	during func()
}

func (p *powerProvider) PowerDown(_ context.Context, _ sandbox.SandboxRef, state []byte, reason sandbox.PowerDownReason) (sandbox.Wake, []byte, error) {
	p.powerDowns = append(p.powerDowns, reason)
	if p.during != nil {
		p.during()
	}
	if p.refuse {
		return sandbox.Wake{}, state, sandbox.ErrPowerDownRefused
	}
	return p.wake, state, nil
}

func (p *powerProvider) Suspend(_ context.Context, _ sandbox.SandboxRef, state []byte) ([]byte, error) {
	p.suspends++
	return state, p.suspendErr
}

func (p *powerProvider) Start(ctx context.Context, ref sandbox.SandboxRef, _ []byte) ([]byte, error) {
	p.startEpochs = append(p.startEpochs, ref.StartEpoch)
	if p.duringStart != nil {
		p.duringStart()
	}
	if p.blockStart {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if p.startErr != nil {
		return nil, p.startErr
	}
	p.starts++
	return nil, nil
}

type powerFixture struct {
	svc        *Service
	st         *store.Store
	provider   *powerProvider
	reconciler *sandboxPowerReconciler
	now        time.Time
	id         string
	sandboxID  string
	seq        int64
}

func newPowerFixture(t *testing.T) *powerFixture {
	t.Helper()
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	provider := &powerProvider{}
	manager := sandbox.NewProviderManager(svc.store)
	manager.RegisterProvider("test", provider)
	manager.SetDefault("test")
	svc.sandboxProviders = manager
	if err := st.CreateSandboxProviderInstance(ctx, &model.SandboxProviderInstance{ID: "prov-t", ProjectID: "project-1", Type: "test", Name: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePool(ctx, &model.Pool{ID: "pool-t", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool-t", ProviderInstanceID: "prov-t"}}); err != nil {
		t.Fatal(err)
	}
	sb := &model.Sandbox{ProjectID: "project-1", PoolID: "pool-t", CreatedByUserID: "user-1", Name: "sleepy",
		SandboxManifest: model.SandboxManifest{Image: "image"}}
	sb.DesiredState = model.DesiredStatePresent
	if err := st.CreateSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	f := &powerFixture{svc: svc, st: st, provider: provider, now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC), sandboxID: sb.ID, id: SandboxDirtyID("project-1", sb.ID)}
	f.reconciler = &sandboxPowerReconciler{store: st, providers: manager, now: func() time.Time { return f.now }}
	return f
}

// poolReady records a ready report from the sandbox's pool at the fixture's
// clock, which leaves it active and ready: a wake is sent only to such a
// pool (ADR 26-11-15-829 §2).
func (f *powerFixture) poolReady(t *testing.T) {
	t.Helper()
	if err := f.st.Transaction(context.Background(), func(_ *store.Store, tx *gorm.DB) error {
		return tx.Model(&model.Pool{}).Where("id = ?", "pool-t").
			Updates(map[string]any{"state": model.PoolStateActive, "ready": true, "status_reported_at": f.now}).Error
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *powerFixture) setRuntime(t *testing.T, state string) {
	t.Helper()
	f.seq++
	if _, err := f.st.ApplySandboxStateReports(context.Background(), store.SandboxStateReportBatch{
		PoolID: "pool-t", BootID: "boot-1", Sequence: f.seq, ReportedAt: f.now,
		Reports: []store.SandboxStateReport{{SandboxID: f.sandboxID, State: state}},
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *powerFixture) report(t *testing.T, autostop map[string]any) {
	t.Helper()
	status, err := json.Marshal(map[string]any{"autostop": autostop})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpdateSandboxAgentStatus(context.Background(), "project-1", f.sandboxID, status, f.now, nil); err != nil {
		t.Fatal(err)
	}
}

func (f *powerFixture) load(t *testing.T) *model.Sandbox {
	t.Helper()
	sb, err := f.st.GetSandbox(context.Background(), "project-1", f.sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

func TestAnIdleSandboxIsStoppedAndItsWakeScheduled(t *testing.T) {
	f := newPowerFixture(t)
	f.setRuntime(t, model.SandboxRuntimeStateRunning)
	f.report(t, map[string]any{"idle": true})
	wakeAt := f.now.Add(time.Hour)
	f.provider.wake = sandbox.Wake{At: wakeAt, Name: "ci"}
	result, err := f.reconciler.Reconcile(context.Background(), f.id)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.provider.powerDowns) != 1 || f.provider.powerDowns[0] != sandbox.PowerDownIdle {
		t.Fatalf("power-downs = %v, want one idle", f.provider.powerDowns)
	}
	if got := f.load(t).WakeAt; got == nil || !got.Equal(wakeAt) {
		t.Fatalf("wake = %v, want %v", got, wakeAt)
	}
	if !result.RequeueAt.Equal(wakeAt) {
		t.Fatalf("requeue at %v, want the wake", result.RequeueAt)
	}
}

// The agent's word is the stronger claim, and only a sleep may be frozen.
func TestSleepWinsOverIdle(t *testing.T) {
	f := newPowerFixture(t)
	f.setRuntime(t, model.SandboxRuntimeStateRunning)
	f.report(t, map[string]any{"idle": true, "sleep": map[string]any{"requestedAt": f.now, "request": "agent"}})
	if _, err := f.reconciler.Reconcile(context.Background(), f.id); err != nil {
		t.Fatal(err)
	}
	if len(f.provider.powerDowns) != 1 || f.provider.powerDowns[0] != sandbox.PowerDownSleep {
		t.Fatalf("power-downs = %v, want one sleep", f.provider.powerDowns)
	}
}

// A refusal is the stored status having gone stale — a client attached since
// — and nothing changes.
func TestARefusedPowerDownChangesNothing(t *testing.T) {
	f := newPowerFixture(t)
	f.setRuntime(t, model.SandboxRuntimeStateRunning)
	f.report(t, map[string]any{"idle": true})
	f.provider.refuse = true
	f.provider.wake = sandbox.Wake{At: f.now.Add(time.Hour)}
	if _, err := f.reconciler.Reconcile(context.Background(), f.id); err != nil {
		t.Fatalf("refused power-down = %v, want settled", err)
	}
	if f.load(t).WakeAt != nil {
		t.Fatal("recorded a wake from a refused power-down")
	}
}

// The stored status outlives the boot that reported it: a stopped sandbox is
// never powered down on it.
func TestAStoppedSandboxIsNotPoweredDownOnItsLastStatus(t *testing.T) {
	f := newPowerFixture(t)
	f.setRuntime(t, model.SandboxRuntimeStateStopped)
	f.report(t, map[string]any{"idle": true})
	if _, err := f.reconciler.Reconcile(context.Background(), f.id); err != nil {
		t.Fatal(err)
	}
	if len(f.provider.powerDowns) != 0 {
		t.Fatalf("power-downs = %v, want none for a stopped sandbox", f.provider.powerDowns)
	}
}

// A person's stop that lands while the power-down is in flight cancels the
// wake before it exists; the reconcile must not write it afterwards.
func TestAStopDuringThePowerDownCancelsTheWake(t *testing.T) {
	f := newPowerFixture(t)
	f.setRuntime(t, model.SandboxRuntimeStateRunning)
	f.report(t, map[string]any{"sleep": map[string]any{"requestedAt": f.now, "request": "agent"}})
	f.provider.wake = sandbox.Wake{At: f.now.Add(time.Hour)}
	f.provider.during = func() {
		if _, err := f.svc.StopSandbox(context.Background(), "project-1", f.sandboxID, services.StopSandboxBody{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.reconciler.Reconcile(context.Background(), f.id); err != nil {
		t.Fatal(err)
	}
	if got := f.load(t).WakeAt; got != nil {
		t.Fatalf("wake = %v after a stop canceled it, want none", got)
	}
}

func TestTheWakeStartsAStoppedSandboxOnce(t *testing.T) {
	f := newPowerFixture(t)
	f.setRuntime(t, model.SandboxRuntimeStateRunning)
	f.report(t, map[string]any{"idle": true})
	f.provider.wake = sandbox.Wake{At: f.now.Add(time.Hour)}
	if _, err := f.reconciler.Reconcile(context.Background(), f.id); err != nil {
		t.Fatal(err)
	}
	f.setRuntime(t, model.SandboxRuntimeStateStopped)
	f.now = f.now.Add(time.Hour)
	f.poolReady(t)
	for range 2 {
		if result, err := f.reconciler.Reconcile(context.Background(), f.id); err != nil || !result.RequeueAt.IsZero() {
			t.Fatalf("reconcile at the wake = %+v, %v, want settled with no timer", result, err)
		}
	}
	if f.provider.starts != 1 {
		t.Fatalf("starts = %d, want exactly one", f.provider.starts)
	}
	if f.load(t).WakeAt != nil {
		t.Fatal("the wake was not spent")
	}
}

// A sandbox the row says is running at its wake time — really running, or
// powered down a moment ago and not yet reported stopped — spends the wake on
// one start, which the pool takes as a no-op if it is running. Leaving the
// wake would requeue the reconcile for now, forever; skipping the start would
// lose a wake that came due while the sandbox powered down.
func TestTheWakeOfASandboxReportedRunningIsSpentOnOneStart(t *testing.T) {
	f := newPowerFixture(t)
	f.setRuntime(t, model.SandboxRuntimeStateRunning)
	f.report(t, map[string]any{"idle": true})
	f.provider.wake = sandbox.Wake{At: f.now.Add(time.Hour)}
	if _, err := f.reconciler.Reconcile(context.Background(), f.id); err != nil {
		t.Fatal(err)
	}
	// Started early by use, still running when the wake comes due.
	f.report(t, map[string]any{"idle": false})
	f.now = f.now.Add(time.Hour)
	f.poolReady(t)
	if result, err := f.reconciler.Reconcile(context.Background(), f.id); err != nil || !result.RequeueAt.IsZero() {
		t.Fatalf("reconcile = %+v, %v, want settled", result, err)
	}
	if f.provider.starts != 1 {
		t.Fatalf("starts = %d, want exactly one", f.provider.starts)
	}
	if f.load(t).WakeAt != nil {
		t.Fatal("the wake of a running sandbox was not spent")
	}
	if result, err := f.reconciler.Reconcile(context.Background(), f.id); err != nil || !result.RequeueAt.IsZero() || f.provider.starts != 1 {
		t.Fatalf("second reconcile = %+v, %v, starts %d, want nothing more", result, err, f.provider.starts)
	}
}

// Only a status that asks for a power-down marks the power reconcile.
func TestOnlyAStatusAskingForAPowerDownMarksIt(t *testing.T) {
	f := newPowerFixture(t)
	for status, want := range map[string]bool{
		`{"autostop":{"idleTimeoutSeconds":60}}`:                   false,
		`{"autostop":{"idle":false}}`:                              false,
		`{"autostop":{"idle":true}}`:                               true,
		`{"autostop":{"sleep":{"requestedAt":"x","request":"a"}}}`: true,
		`{}`: false,
	} {
		_, got := powerDownAsked(json.RawMessage(status))
		if got != want {
			t.Errorf("powerDownAsked(%s) = %v, want %v", status, got, want)
		}
	}
	_ = f
}

// A person's suspend is an instruction from outside: it reaches the runtime
// and cancels the wake the sandbox asked for, as a stop does
// (ADR 26-10-16-420 §3).
func TestASuspendReachesTheRuntimeAndCancelsTheWake(t *testing.T) {
	f := newPowerFixture(t)
	f.setRuntime(t, model.SandboxRuntimeStateRunning)
	at := f.now.Add(time.Hour)
	if _, err := f.st.SetSandboxWakeIfEpoch(context.Background(), "project-1", f.sandboxID, 0, &at); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SuspendSandbox(context.Background(), "project-1", f.sandboxID, services.SuspendSandboxBody{}); err != nil {
		t.Fatal(err)
	}
	if f.provider.suspends != 1 {
		t.Fatalf("suspends = %d, want 1", f.provider.suspends)
	}
	if got := f.load(t).WakeAt; got != nil {
		t.Fatalf("wake = %v after a suspend, want it canceled", got)
	}
}

// A frozen sandbox's last status still asks for the sleep that froze it. It
// is not running, so it is not asked again; its wake still starts it, which
// is what thaws it (ADR 26-10-16-420 §4).
func TestASuspendedSandboxIsNotPoweredDownAndItsWakeStartsIt(t *testing.T) {
	f := newPowerFixture(t)
	f.setRuntime(t, model.SandboxRuntimeStateRunning)
	f.report(t, map[string]any{"sleep": map[string]any{"requestedAt": f.now, "request": "agent"}})
	f.provider.wake = sandbox.Wake{At: f.now.Add(time.Hour)}
	if _, err := f.reconciler.Reconcile(context.Background(), f.id); err != nil {
		t.Fatal(err)
	}
	f.setRuntime(t, model.SandboxRuntimeStateSuspended)
	if _, err := f.reconciler.Reconcile(context.Background(), f.id); err != nil {
		t.Fatal(err)
	}
	if len(f.provider.powerDowns) != 1 {
		t.Fatalf("power-downs = %v, want the one that froze it", f.provider.powerDowns)
	}
	f.now = f.now.Add(time.Hour)
	f.poolReady(t)
	if _, err := f.reconciler.Reconcile(context.Background(), f.id); err != nil {
		t.Fatal(err)
	}
	if f.provider.starts != 1 {
		t.Fatalf("starts = %d at the wake, want 1", f.provider.starts)
	}
}

// A refused suspend changes nothing, the wake included: the likeliest refusal
// is a sandbox a sleep stopped, which is the one holding a wake
// (ADR 26-10-16-420 §3).
func TestARefusedSuspendKeepsTheWake(t *testing.T) {
	f := newPowerFixture(t)
	f.setRuntime(t, model.SandboxRuntimeStateStopped)
	at := f.now.Add(time.Hour)
	if _, err := f.st.SetSandboxWakeIfEpoch(context.Background(), "project-1", f.sandboxID, 0, &at); err != nil {
		t.Fatal(err)
	}
	f.provider.suspendErr = sandbox.ErrNotRunning
	if _, err := f.svc.SuspendSandbox(context.Background(), "project-1", f.sandboxID, services.SuspendSandboxBody{}); err == nil {
		t.Fatal("suspend of a stopped sandbox succeeded")
	}
	if got := f.load(t).WakeAt; got == nil || !got.Equal(at) {
		t.Fatalf("wake = %v after a refused suspend, want %v kept", got, at)
	}
}

func (p *powerProvider) PullSource(context.Context, sandbox.SandboxRef, sourcecache.PullRequest) (sourcecache.Delivery, error) {
	panic("unexpected source pull")
}

// sleptWithDueWake has the fixture's sandbox stopped on its own request with a
// wake that is now due, and its pool ready.
func (f *powerFixture) sleptWithDueWake(t *testing.T) time.Time {
	t.Helper()
	f.setRuntime(t, model.SandboxRuntimeStateRunning)
	f.report(t, map[string]any{"idle": true})
	wakeAt := f.now.Add(time.Hour)
	f.provider.wake = sandbox.Wake{At: wakeAt}
	if _, err := f.reconciler.Reconcile(context.Background(), f.id); err != nil {
		t.Fatal(err)
	}
	f.setRuntime(t, model.SandboxRuntimeStateStopped)
	f.now = wakeAt
	f.poolReady(t)
	return wakeAt
}

// A start the pool answered with a refusal of its own spends the wake, and
// nothing sends it again: retrying a start the runtime refused is the loop
// ADR 26-10-12-711 rejects (ADR 26-11-15-829 §1).
func TestAWakeWhoseStartThePoolRefusedIsSpent(t *testing.T) {
	f := newPowerFixture(t)
	f.sleptWithDueWake(t)
	f.provider.startErr = errors.New("container failed to start")
	for range 2 {
		if result, err := f.reconciler.Reconcile(context.Background(), f.id); err != nil || !result.RequeueAt.IsZero() {
			t.Fatalf("reconcile at the wake = %+v, %v, want settled", result, err)
		}
	}
	if len(f.provider.startEpochs) != 1 {
		t.Fatalf("start attempts = %d, want the one the pool refused", len(f.provider.startEpochs))
	}
	if f.load(t).WakeAt != nil {
		t.Fatal("a wake whose start the pool refused was kept")
	}
}

// A due wake is not sent while its pool is not active and ready: it is kept
// and looked at again on a requeue, which is no failure (§2).
func TestAWakeIsNotSentToAPoolThatIsNotReady(t *testing.T) {
	f := newPowerFixture(t)
	wakeAt := f.sleptWithDueWake(t)
	// The server restarted: the pool has not reported to it yet.
	if err := f.st.Transaction(context.Background(), func(_ *store.Store, tx *gorm.DB) error {
		return tx.Model(&model.Pool{}).Where("id = ?", "pool-t").Update("health_check_started_at", f.now.Add(time.Second)).Error
	}); err != nil {
		t.Fatal(err)
	}
	result, err := f.reconciler.Reconcile(context.Background(), f.id)
	if err != nil {
		t.Fatalf("reconcile = %v, want a wait, not a failure", err)
	}
	if !result.RequeueAt.Equal(f.now.Add(wakePoolWait)) {
		t.Fatalf("requeue at %v, want %v", result.RequeueAt, f.now.Add(wakePoolWait))
	}
	if len(f.provider.startEpochs) != 0 {
		t.Fatalf("start attempts = %d to a pool that is not ready, want none", len(f.provider.startEpochs))
	}
	if got := f.load(t).WakeAt; got == nil || !got.Equal(wakeAt) {
		t.Fatalf("wake = %v, want %v kept", got, wakeAt)
	}
}

// A wake whose start is in flight when the server stops is kept: the pool did
// not take it, so the next server sends it (§1).
func TestAWakeInFlightWhenTheServerStopsIsKept(t *testing.T) {
	f := newPowerFixture(t)
	f.sleptWithDueWake(t)
	f.provider.blockStart = true
	ctx, stop := context.WithCancel(context.Background())
	f.provider.duringStart = stop
	if _, err := f.reconciler.Reconcile(ctx, f.id); err == nil {
		t.Fatal("a wake cut off by the server stopping settled; want it kept and retried")
	}
	if f.load(t).WakeAt == nil {
		t.Fatal("the wake was spent on a start the server stopped before the pool took")
	}
}

// The wake's start is fenced at the pool on the epoch read with it, and is
// sent again on that epoch until the pool takes it. A person's stop after a
// lost first attempt moves the epoch, so that attempt, replayed, is refused at
// the pool, and the reconcile sends nothing more (§3).
func TestTheWakeStartIsFencedOnItsEpoch(t *testing.T) {
	f := newPowerFixture(t)
	f.sleptWithDueWake(t)
	epoch := f.load(t).WakeEpoch
	f.provider.startErr = fmt.Errorf("pool pool-t: %w", sandbox.ErrPoolNotReachable)
	if _, err := f.reconciler.Reconcile(context.Background(), f.id); err == nil {
		t.Fatal("a wake the pool did not take settled; want it retried")
	}
	if len(f.provider.startEpochs) != 1 || f.provider.startEpochs[0] == nil || *f.provider.startEpochs[0] != epoch {
		t.Fatalf("start fenced on %v, want epoch %d", f.provider.startEpochs, epoch)
	}
	if f.load(t).WakeAt == nil {
		t.Fatal("the wake was spent on a start the pool never took")
	}
	if _, err := f.svc.cancelSandboxWake(context.Background(), "project-1", f.sandboxID); err != nil {
		t.Fatal(err)
	}
	if moved := f.load(t).WakeEpoch; moved == epoch {
		t.Fatal("a person's stop did not move the epoch the lost attempt was fenced on")
	}
	f.provider.startErr = nil
	if result, err := f.reconciler.Reconcile(context.Background(), f.id); err != nil || !result.RequeueAt.IsZero() {
		t.Fatalf("reconcile after the stop = %+v, %v, want settled", result, err)
	}
	if len(f.provider.startEpochs) != 1 {
		t.Fatalf("start attempts = %d after a person's stop overrode the wake, want none more", len(f.provider.startEpochs))
	}
}

// Spending a wake clears only the wake the start was sent for: a later one,
// written by a power-down that landed while the start was in flight, stands.
func TestSpendingAWakeKeepsALaterOne(t *testing.T) {
	f := newPowerFixture(t)
	f.sleptWithDueWake(t)
	later := f.now.Add(2 * time.Hour).UTC()
	f.provider.duringStart = func() {
		if _, err := f.st.SetSandboxWakeIfEpoch(context.Background(), "project-1", f.sandboxID, f.load(t).WakeEpoch, &later); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.reconciler.Reconcile(context.Background(), f.id); err != nil {
		t.Fatal(err)
	}
	if got := f.load(t).WakeAt; got == nil || !got.Equal(later) {
		t.Fatalf("wake = %v, want the later %v kept", got, later)
	}
}

// A fence refusal that finds the wake still recorded is the pool's
// authorization callback failing — a server busy or just restarted — not the
// wake being superseded, which would have cleared it: the wake is kept and
// sent again (§1).
func TestAWakeRefusedByAFailingFenceIsKept(t *testing.T) {
	f := newPowerFixture(t)
	f.sleptWithDueWake(t)
	f.provider.startErr = fmt.Errorf("%w: authorize start: 503", sandbox.ErrStartNotAuthorized)
	if _, err := f.reconciler.Reconcile(context.Background(), f.id); err == nil {
		t.Fatal("a wake the fence could not authorize settled; want it kept and retried")
	}
	if f.load(t).WakeAt == nil {
		t.Fatal("the wake was spent on a start the pool never ran")
	}
	f.provider.startErr = nil
	if _, err := f.reconciler.Reconcile(context.Background(), f.id); err != nil {
		t.Fatal(err)
	}
	if f.provider.starts != 1 || f.load(t).WakeAt != nil {
		t.Fatalf("starts = %d, wake = %v; want one start and the wake spent", f.provider.starts, f.load(t).WakeAt)
	}
}

// A wake its pool keeps not taking does not hold the sandbox up: a sandbox a
// use started that reports itself idle is still powered down, and the wake is
// kept for its pool.
func TestAKeptWakeDoesNotHoldUpAPowerDown(t *testing.T) {
	f := newPowerFixture(t)
	f.sleptWithDueWake(t)
	// Started by use, and idle again; the pool's agent cannot take a fenced
	// start yet.
	f.setRuntime(t, model.SandboxRuntimeStateRunning)
	f.report(t, map[string]any{"idle": true})
	f.provider.startErr = fmt.Errorf("%w: waiting for pool pool-t to upgrade its agent for explicit starts", sandbox.ErrPoolNotReachable)
	if _, err := f.reconciler.Reconcile(context.Background(), f.id); err == nil {
		t.Fatal("a wake the pool did not take settled; want it kept and retried")
	}
	if len(f.provider.powerDowns) != 2 {
		t.Fatalf("power-downs = %v, want the idle sandbox powered down again", f.provider.powerDowns)
	}
	if f.load(t).WakeAt == nil {
		t.Fatal("the wake was dropped")
	}
}

// A sandbox a use started while its wake was kept for its pool boots and is
// handed that wake, which moves it aside; its next power-down's confirmation
// then carries no wake, and that clears the row, so the wake its boot already
// delivered is not sent again once the pool takes starts (ADR 26-10-16-337
// §§5, 7).
func TestAWakeItsBootDeliveredIsNotSentAgain(t *testing.T) {
	f := newPowerFixture(t)
	f.sleptWithDueWake(t)
	f.setRuntime(t, model.SandboxRuntimeStateRunning)
	f.report(t, map[string]any{"idle": true})
	f.provider.wake = sandbox.Wake{}
	f.provider.startErr = fmt.Errorf("%w: waiting for pool pool-t to upgrade its agent for explicit starts", sandbox.ErrPoolNotReachable)
	if result, err := f.reconciler.Reconcile(context.Background(), f.id); err != nil || !result.RequeueAt.IsZero() {
		t.Fatalf("reconcile = %+v, %v, want settled once the confirmation cleared the wake", result, err)
	}
	if len(f.provider.powerDowns) != 2 {
		t.Fatalf("power-downs = %v, want the idle sandbox powered down", f.provider.powerDowns)
	}
	if got := f.load(t).WakeAt; got != nil {
		t.Fatalf("wake = %v after a confirmation that carried none, want it cleared", got)
	}
	f.provider.startErr = nil
	f.setRuntime(t, model.SandboxRuntimeStateStopped)
	if _, err := f.reconciler.Reconcile(context.Background(), f.id); err != nil {
		t.Fatal(err)
	}
	if f.provider.starts != 0 {
		t.Fatalf("starts = %d for a wake already delivered, want none", f.provider.starts)
	}
}
