package pools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// scriptedPoolProvider records each runtime call and answers it with fail,
// when set. Every call reports work evidence, so a test can see which attempt
// the call's executor was bound to.
type scriptedPoolProvider struct {
	stubPoolProvider
	fail     error
	calls    []string
	reported []string
}

func (p *scriptedPoolProvider) call(ctx context.Context, kind string) error {
	p.calls = append(p.calls, kind)
	if facts, err := sandbox.ReportBootstrap(ctx, model.BootstrapEvidence{Operation: "image:test", Bytes: int64(len(p.calls))}); err == nil && facts != nil {
		p.reported = append(p.reported, facts.Attempt)
	}
	return p.fail
}

func (p *scriptedPoolProvider) ReconcilePool(ctx context.Context, _ sandbox.PoolManager, _ *model.Project, _ *model.SandboxProviderInstance, _ *model.Pool, _ func(context.Context) error) error {
	return p.call(ctx, "reconcile")
}

func (p *scriptedPoolProvider) RepairPool(ctx context.Context, _ sandbox.PoolManager, _ *model.Project, _ *model.SandboxProviderInstance, _ *model.Pool, _ string, _ func(context.Context) error) error {
	return p.call(ctx, "repair")
}

type bootstrapRetryFixture struct {
	t          *testing.T
	store      *store.Store
	db         *gorm.DB
	runtime    *scriptedPoolProvider
	reconciler *PoolReconciler
}

// newBootstrapRetryFixture creates a pool the way the API does, so it carries
// the attempt Pool.BeforeCreate starts and has never registered.
func newBootstrapRetryFixture(t *testing.T) *bootstrapRetryFixture {
	t.Helper()
	ctx := context.Background()
	appStore, db := newPoolReconcilerTestStore(t)
	runtime := &scriptedPoolProvider{}
	manager := sandbox.NewProviderManager(appStore)
	manager.RegisterProvider("stub", runtime)
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "stub", Name: "stub"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: provider.ID}}
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	return &bootstrapRetryFixture{t: t, store: appStore, db: db, runtime: runtime, reconciler: NewPoolReconciler(appStore, manager, NewControlPlane(appStore, nil))}
}

func (f *bootstrapRetryFixture) reconcile() error {
	f.t.Helper()
	_, err := f.reconcileResult()
	return err
}

func (f *bootstrapRetryFixture) reconcileResult() (reconcile.Result, error) {
	f.t.Helper()
	return f.reconciler.Reconcile(context.Background(), PoolDirtyID("project-1", "pool-1"))
}

// backoffPassed moves the ended attempt's end far enough back that the pacing
// before the next attempt has passed.
func (f *bootstrapRetryFixture) backoffPassed() {
	f.t.Helper()
	b := f.attempt()
	if b.Failure == "" {
		f.t.Fatal("attempt has not failed")
	}
	b.EndedAt = time.Now().Add(-reconcile.DefaultBackoffMax - time.Second)
	f.setAttempt(b)
}

func (f *bootstrapRetryFixture) pool() *model.Pool {
	f.t.Helper()
	pool, err := f.store.GetPool(context.Background(), "project-1", "pool-1")
	if err != nil {
		f.t.Fatalf("get pool: %v", err)
	}
	return pool
}

func (f *bootstrapRetryFixture) attempt() *model.PoolBootstrap {
	f.t.Helper()
	b := f.pool().BootstrapFacts()
	if b == nil {
		f.t.Fatal("pool has no bootstrap attempt")
	}
	return b
}

func (f *bootstrapRetryFixture) column(name string, value any) {
	f.t.Helper()
	if err := f.db.Model(&model.Pool{}).Where("id = ?", "pool-1").UpdateColumn(name, value).Error; err != nil {
		f.t.Fatalf("set %s: %v", name, err)
	}
}

func (f *bootstrapRetryFixture) setAttempt(b *model.PoolBootstrap) {
	f.t.Helper()
	data, err := json.Marshal(b)
	if err != nil {
		f.t.Fatal(err)
	}
	f.column("bootstrap", model.RawJSON(data))
}

// The issue's case: one provider error used to be the attempt's permanent
// failure, and a never-registered pool stayed `failed` until its row was
// re-created. The failing pass still settles failed, and the next pass starts
// a new attempt by replacing the runtime.
func TestNeverRegisteredPoolRetriesAfterAProviderError(t *testing.T) {
	f := newBootstrapRetryFixture(t)
	first := f.attempt()

	f.runtime.fail = errors.New("image discobox-platform:local not found")
	if err := f.reconcile(); err == nil {
		t.Fatal("failing pass settled cleanly: the engine would not back off before the next attempt")
	}
	failed := f.pool()
	if failed.State != model.PoolStateFailed || failed.ErrorMessage == nil {
		t.Fatalf("state = %q, error %v; want failed with the reason", failed.State, failed.ErrorMessage)
	}
	ended := f.attempt()
	if ended.Attempt != first.Attempt || !strings.Contains(ended.Failure, "not found") {
		t.Fatalf("attempt %q failure %q; want the first attempt to carry the error, and no new attempt on the failing pass", ended.Attempt, ended.Failure)
	}

	// The next attempt waits out the engine's first backoff step.
	result, err := f.reconcileResult()
	if err != nil || result.RequeueAt.IsZero() || len(f.runtime.calls) != 1 {
		t.Fatalf("pass inside the backoff: result=%+v err=%v calls=%v; want a requeue and no provider call", result, err, f.runtime.calls)
	}
	if b := f.attempt(); b.Attempt != first.Attempt {
		t.Fatal("an attempt started inside the backoff")
	}

	f.runtime.fail = nil
	f.backoffPassed()
	if err := f.reconcile(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := strings.Join(f.runtime.calls, ","); got != "reconcile,repair" {
		t.Fatalf("calls = %s; want the new attempt's first call to be a repair", got)
	}
	second := f.attempt()
	if second.Attempt == first.Attempt || second.Failure != "" || second.Generation != first.Generation || second.Failed != 1 {
		t.Fatalf("attempt after retry = %+v; want a fresh attempt for generation %d after one failure", second, first.Generation)
	}
	if second.Repair != "" {
		t.Fatalf("repair still owed after it succeeded: %q", second.Repair)
	}
	if f.runtime.reported[len(f.runtime.reported)-1] != second.Attempt {
		t.Fatal("the repair's work evidence was not bound to the new attempt")
	}
	recovered := f.pool()
	if recovered.State != model.PoolStateRegistering || recovered.ErrorMessage != nil {
		t.Fatalf("state = %q, error %v; want registering with the failure cleared", recovered.State, recovered.ErrorMessage)
	}

	// The ended attempt's executor can no longer write evidence.
	if _, err := f.store.RecordPoolBootstrap(context.Background(), "pool-1", first.Attempt, first.Generation, model.BootstrapEvidence{Operation: "apply", Complete: true}); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("report for the ended attempt = %v, want a conflict", err)
	}

	f.column("registered_at", time.Now().UTC())
	if err := f.reconcile(); err != nil {
		t.Fatalf("reconcile after registration: %v", err)
	}
	if got := f.pool().State; got != model.PoolStateActive {
		t.Fatalf("state = %q, want active once the agent registered", got)
	}
}

// The dev-loop trigger: the attempt's silence budget ran out while the server
// was down, so the pass that finds it records the expiry without calling the
// provider, and the next pass starts again.
func TestNeverRegisteredPoolRetriesAfterItsAttemptExpired(t *testing.T) {
	f := newBootstrapRetryFixture(t)
	first := f.attempt()
	first.ProgressAt = time.Now().Add(-model.PoolBootstrapSilence - time.Second)
	f.setAttempt(first)

	err := f.reconcile()
	if err == nil || !strings.Contains(err.Error(), "bootstrap stopped making progress") {
		t.Fatalf("expired pass = %v, want the expiry", err)
	}
	if len(f.runtime.calls) != 0 {
		t.Fatalf("calls = %v; the pass that records expiry calls no provider", f.runtime.calls)
	}
	if b := f.attempt(); b.Attempt != first.Attempt || b.Failure == "" {
		t.Fatalf("attempt = %+v, want the first attempt with its expiry recorded", b)
	}
	if got := f.pool().State; got != model.PoolStateFailed {
		t.Fatalf("state = %q, want failed", got)
	}

	f.backoffPassed()
	if err := f.reconcile(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := strings.Join(f.runtime.calls, ","); got != "repair" {
		t.Fatalf("calls = %s, want one repair", got)
	}
	if b := f.attempt(); b.Attempt == first.Attempt || b.Failure != "" {
		t.Fatalf("attempt = %+v, want a fresh one", b)
	}
	if got := f.pool().State; got != model.PoolStateRegistering {
		t.Fatalf("state = %q, want registering", got)
	}
}

// A cause that never clears keeps failing, one attempt per pass once its
// pacing has passed, and the pacing grows with every failed attempt.
func TestNeverRegisteredPoolKeepsRetryingACauseThatPersists(t *testing.T) {
	f := newBootstrapRetryFixture(t)
	f.runtime.fail = errors.New("pull access denied")
	seen := map[string]bool{}
	var lastWait time.Duration
	for pass := 0; pass < 4; pass++ {
		if pass > 0 {
			result, err := f.reconcileResult()
			if err != nil || result.RequeueAt.IsZero() {
				t.Fatalf("pass %d inside the backoff: result=%+v err=%v", pass, result, err)
			}
			wait := result.RequeueAt.Sub(f.attempt().EndedAt)
			if wait <= lastWait {
				t.Fatalf("pass %d waits %s after %s: the pacing did not grow", pass, wait, lastWait)
			}
			lastWait = wait
			f.backoffPassed()
		}
		if err := f.reconcile(); err == nil {
			t.Fatalf("pass %d settled cleanly", pass)
		}
		b := f.attempt()
		if seen[b.Attempt] {
			t.Fatalf("pass %d reused attempt %s", pass, b.Attempt)
		}
		seen[b.Attempt] = true
		if b.Failure == "" || b.Failed != pass {
			t.Fatalf("pass %d attempt = %+v, want failed with %d failures before it", pass, b, pass)
		}
	}
	if got := strings.Join(f.runtime.calls, ","); got != "reconcile,repair,repair,repair" {
		t.Fatalf("calls = %s", got)
	}
}

// A repair that succeeds settles its pass cleanly, which resets the engine's
// own failure count. An agent that then never registers still has its
// attempts paced, because the count lives on the attempt.
func TestAttemptsWhoseAgentNeverRegistersArePaced(t *testing.T) {
	f := newBootstrapRetryFixture(t)
	expire := func() {
		b := f.attempt()
		b.ProgressAt = time.Now().Add(-model.PoolBootstrapSilence - time.Second)
		f.setAttempt(b)
	}
	for round := 0; round < 3; round++ {
		expire()
		if err := f.reconcile(); err == nil {
			t.Fatalf("round %d: expiry settled cleanly", round)
		}
		b := f.attempt()
		if b.Failed != round {
			t.Fatalf("round %d: %d failures before this attempt, want %d", round, b.Failed, round)
		}
		result, err := f.reconcileResult()
		if err != nil || result.RequeueAt.IsZero() {
			t.Fatalf("round %d: pass inside the backoff: result=%+v err=%v", round, result, err)
		}
		if want := b.EndedAt.Add(reconcile.Backoff(reconcile.DefaultBackoffBase, reconcile.DefaultBackoffMax, round)); !result.RequeueAt.Equal(want) {
			t.Fatalf("round %d: next attempt due %s, want %s", round, result.RequeueAt, want)
		}
		f.backoffPassed()
		if err := f.reconcile(); err != nil {
			t.Fatalf("round %d: repair: %v", round, err)
		}
	}
	if got := strings.Join(f.runtime.calls, ","); got != "repair,repair,repair" {
		t.Fatalf("calls = %s", got)
	}
}

// The new attempt is stored before its repair runs. A pass that dies in
// between leaves the repair owed, and the next pass repairs rather than
// reconciling the untrusted runtime in place.
func TestAnOwedRepairSurvivesAPassThatDied(t *testing.T) {
	f := newBootstrapRetryFixture(t)
	first := f.attempt()
	if _, err := f.store.RecordPoolBootstrap(context.Background(), "pool-1", first.Attempt, first.Generation, model.BootstrapEvidence{Failure: "image not found"}); err != nil {
		t.Fatal(err)
	}
	// The dying pass got as far as this.
	if _, err := f.store.ReplacePoolBootstrap(context.Background(), "pool-1", first.Attempt, first.Generation); err != nil {
		t.Fatal(err)
	}
	if b := f.attempt(); !strings.Contains(b.Repair, "image not found") {
		t.Fatalf("new attempt owes %q, want a repair naming the failure", b.Repair)
	}
	if err := f.reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := f.reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := strings.Join(f.runtime.calls, ","); got != "repair,reconcile" {
		t.Fatalf("calls = %s, want the owed repair once and then an in-place reconcile", got)
	}
}

// New intent on a never-registered pool gets an attempt of its own, rather
// than leaving the pool on the legacy registration policy for good.
func TestNeverRegisteredPoolGetsAnAttemptForNewIntent(t *testing.T) {
	f := newBootstrapRetryFixture(t)
	first := f.attempt()
	f.column("generation", first.Generation+1)

	if err := f.reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	b := f.attempt()
	if b.Attempt == first.Attempt || b.Generation != first.Generation+1 {
		t.Fatalf("attempt = %+v, want a fresh one for generation %d", b, first.Generation+1)
	}
	if got := strings.Join(f.runtime.calls, ","); got != "repair" {
		t.Fatalf("calls = %s, want one repair", got)
	}
}

// New intent is not held by the pacing of failures under the old intent.
func TestNewIntentIsNotPacedByAFailedAttempt(t *testing.T) {
	f := newBootstrapRetryFixture(t)
	first := f.attempt()
	first.Failure = "image not found"
	first.EndedAt = time.Now()
	first.Failed = 20
	f.setAttempt(first)
	f.column("generation", first.Generation+1)

	if err := f.reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := strings.Join(f.runtime.calls, ","); got != "repair" {
		t.Fatalf("calls = %s, want a repair on the first pass for the new intent", got)
	}
	if b := f.attempt(); b.Generation != first.Generation+1 || b.Failed != 0 {
		t.Fatalf("attempt = %+v, want one for the new generation with its count started over", b)
	}
}

// A provider that cannot be called starts no attempt: the next pass that can
// call it does, so the new attempt's first call is still the repair.
func TestBootstrapRetryWaitsForACallableProvider(t *testing.T) {
	f := newBootstrapRetryFixture(t)
	first := f.attempt()
	first.Failure = "image not found"
	f.setAttempt(first)
	if err := f.db.Model(&model.SandboxProviderInstance{}).Where("id = ?", "provider-1").UpdateColumn("disabled", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err != nil {
		t.Fatalf("reconcile with a disabled provider: %v", err)
	}
	if b := f.attempt(); b.Attempt != first.Attempt {
		t.Fatal("an attempt started with no provider to call")
	}
}

// Neither a registered pool nor a legacy row without a bootstrap document
// ever gets an attempt.
func TestBootstrapRetryLeavesRegisteredAndLegacyPoolsAlone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*bootstrapRetryFixture)
	}{
		{"registered", func(f *bootstrapRetryFixture) {
			b := f.attempt()
			b.Failure = "image not found"
			f.setAttempt(b)
			f.column("registered_at", time.Now().UTC())
		}},
		{"legacy", func(f *bootstrapRetryFixture) { f.column("bootstrap", nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBootstrapRetryFixture(t)
			tc.setup(f)
			before := f.pool().Bootstrap
			if err := f.reconcile(); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if got := strings.Join(f.runtime.calls, ","); got != "reconcile" {
				t.Fatalf("calls = %s, want an in-place reconcile", got)
			}
			if after := f.pool().Bootstrap; string(after) != string(before) {
				t.Fatalf("bootstrap document changed: %s -> %s", before, after)
			}
		})
	}
}

// A repair's error ends the attempt even when it reads as a host coming up:
// it may come before the repair replaced anything, and reconciling in place
// would trust the runtime the repair was owed for.
func TestARepairThatFindsTheHostComingUpEndsTheAttempt(t *testing.T) {
	f := newBootstrapRetryFixture(t)
	first := f.attempt()
	first.Failure = "image not found"
	f.setAttempt(first)
	f.runtime.fail = fmt.Errorf("%w: pool machine is not answering", sandbox.ErrPoolNotReachable)
	if err := f.reconcile(); err == nil {
		t.Fatal("repair error settled cleanly")
	}
	second := f.attempt()
	if second.Attempt == first.Attempt || second.Failure == "" || second.Repair == "" {
		t.Fatalf("attempt = %+v, want a new attempt, failed, with its repair still owed", second)
	}
	f.runtime.fail = nil
	f.backoffPassed()
	if err := f.reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := strings.Join(f.runtime.calls, ","); got != "repair,repair" {
		t.Fatalf("calls = %s, want the next attempt to repair again", got)
	}
}
