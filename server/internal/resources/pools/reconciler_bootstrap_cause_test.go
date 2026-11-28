package pools

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// A provider whose service the server cannot reach is waited on, not failed
// (ErrPoolNotReachable), so nothing records why the attempt makes no
// progress. When its silence budget runs out, the pool's error must still
// name the provider error it was waiting on, not only that bootstrap stopped.
func TestExpiredBootstrapKeepsTheUnreachableProvidersCause(t *testing.T) {
	f := newBootstrapRetryFixture(t)
	cause := errors.New(`boxd: exchange API key: Post "https://app.boxd.sh/api/v1/auth/token": dial tcp 1.2.3.4:443: connect: network is unreachable`)
	f.runtime.fail = fmt.Errorf("pool pool-1: the boxd driver's account cannot be checked yet: %w: %w", cause, sandbox.ErrPoolNotReachable)

	result, err := f.reconcileResult()
	if err != nil || result.RequeueAt.IsZero() {
		t.Fatalf("unreachable pass: result=%+v err=%v; want a requeue, the host waited on", result, err)
	}

	b := f.attempt()
	b.ProgressAt = time.Now().Add(-model.PoolBootstrapSilence - time.Second)
	f.setAttempt(b)
	_ = f.reconcile()

	pool := f.pool()
	if pool.State != model.PoolStateFailed || pool.ErrorMessage == nil {
		t.Fatalf("state = %q, error %v; want failed with a reason", pool.State, pool.ErrorMessage)
	}
	if !strings.Contains(*pool.ErrorMessage, "network is unreachable") {
		t.Fatalf("errorMessage = %q; want it to keep the provider error the attempt was waiting on", *pool.ErrorMessage)
	}
}

// A wait a later pass got past is not the cause: a pass that waits on
// nothing clears it, so an attempt that then expires does not blame it.
func TestExpiredBootstrapDoesNotNameAWaitALaterPassGotPast(t *testing.T) {
	f := newBootstrapRetryFixture(t)
	f.runtime.fail = fmt.Errorf("pool pool-1: container starting: %w", sandbox.ErrPoolNotReachable)
	if result, err := f.reconcileResult(); err != nil || result.RequeueAt.IsZero() {
		t.Fatalf("unreachable pass: result=%+v err=%v; want a requeue", result, err)
	}
	if b := f.attempt(); !strings.Contains(b.Waiting, "container starting") {
		t.Fatalf("waiting = %q, want the wait recorded", b.Waiting)
	}

	f.runtime.fail, f.runtime.silent = nil, true
	_ = f.reconcile()
	b := f.attempt()
	if b.Waiting != "" {
		t.Fatalf("waiting = %q after a pass that waited on nothing, want it cleared", b.Waiting)
	}

	b.ProgressAt = time.Now().Add(-model.PoolBootstrapSilence - time.Second)
	b.RegistrationDeadline = time.Time{}
	b.Deadline = time.Time{}
	f.setAttempt(b)
	_ = f.reconcile()
	pool := f.pool()
	if pool.ErrorMessage == nil || strings.Contains(*pool.ErrorMessage, "container starting") {
		t.Fatalf("errorMessage = %v; want the expiry without the wait a later pass got past", pool.ErrorMessage)
	}
}

// A pool whose box moved nothing because a slot still drains from its last
// handover failed nothing: the pass records no failure and asks again.
func TestASlotStillDrainingIsAskedAgainNotFailed(t *testing.T) {
	f := newBootstrapRetryFixture(t)
	f.runtime.fail = fmt.Errorf("pool pool-1: start the pool's agent: %w", sandbox.ErrPoolSlotDraining)
	result, err := f.reconcileResult()
	if err != nil || result.RequeueAt.IsZero() {
		t.Fatalf("draining pass: result=%+v err=%v; want a requeue and no error", result, err)
	}
	if pool := f.pool(); pool.State == model.PoolStateFailed || pool.ErrorMessage != nil {
		t.Fatalf("state = %q, error %v; want no failure recorded", pool.State, pool.ErrorMessage)
	}
}

// A failed reconcile ends a drain streak: a drain after it is timed from its
// own first pass, not the earlier streak's.
func TestAFailedReconcileEndsADrainStreak(t *testing.T) {
	f := newBootstrapRetryFixture(t)
	f.runtime.fail = fmt.Errorf("pool pool-1: start the pool's agent: %w", sandbox.ErrPoolSlotDraining)
	if _, err := f.reconcileResult(); err != nil {
		t.Fatal(err)
	}
	watch := f.reconciler.pools.Upgrades()
	watch.mu.Lock()
	_, began := watch.draining["pool-1"]
	watch.mu.Unlock()
	if !began {
		t.Fatal("a pass that waited on a drain began no streak")
	}
	f.runtime.fail = fmt.Errorf("pool pool-1: %w: the bundle did not apply", sandbox.ErrPoolConfigApply)
	_, _ = f.reconcileResult()
	watch.mu.Lock()
	_, still := watch.draining["pool-1"]
	watch.mu.Unlock()
	if still {
		t.Fatal("a failed reconcile left the earlier drain streak running")
	}
}

// A pool whose last handover's new agent never took over failed: the pass
// records why, where its status and health show it, and is no drain to wait
// on.
func TestATakeOverThatStalledIsRecordedAsAFailure(t *testing.T) {
	f := newBootstrapRetryFixture(t)
	f.runtime.fail = fmt.Errorf("pool pool-1: %w: %w: start the pool's agent", sandbox.ErrPoolConfigApply, sandbox.ErrPoolTakeOverStalled)
	if _, err := f.reconcileResult(); err == nil {
		t.Fatal("a pass whose new agent never took over returned no error")
	}
	pool := f.pool()
	if pool.ErrorMessage == nil || !strings.Contains(*pool.ErrorMessage, "never took over") {
		t.Fatalf("errorMessage = %v; want it to say the new agent never took over", pool.ErrorMessage)
	}
	watch := f.reconciler.pools.Upgrades()
	watch.mu.Lock()
	_, draining := watch.draining["pool-1"]
	watch.mu.Unlock()
	if draining {
		t.Fatal("a stalled takeover reads as a drain under way")
	}
}
