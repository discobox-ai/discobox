package sandboxes

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// powerProvider records power-downs and starts, and answers a power-down with
// what the test sets.
type powerProvider struct {
	recordingProvider
	powerDowns []sandbox.PowerDownReason
	starts     int
	suspends   int
	suspendErr error
	wake       sandbox.Wake
	refuse     bool
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

func (p *powerProvider) Start(context.Context, sandbox.SandboxRef, []byte) ([]byte, error) {
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
	manager := sandbox.NewProviderManager()
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
		SandboxManifest: model.SandboxManifest{HarnessMode: "run", Image: "image"}}
	sb.DesiredState = model.DesiredStatePresent
	if err := st.CreateSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	f := &powerFixture{svc: svc, st: st, provider: provider, now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC), sandboxID: sb.ID, id: SandboxDirtyID("project-1", sb.ID)}
	f.reconciler = &sandboxPowerReconciler{store: st, providers: manager, now: func() time.Time { return f.now }}
	return f
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
