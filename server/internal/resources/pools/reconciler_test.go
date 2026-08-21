package pools

import (
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// TestArmRegistrationTimeout pins both halves of the deadline the pool
// reconciler owns.
//
// The "already registered" case is the one with teeth: the reconciler
// drift-checks every healthy pool in the fleet, so arming there would put a
// standing timer on every pool and turn a quiet fleet into steady reconcile
// traffic. It used to be armed a layer down, in the pool runtime provider,
// where the only thing stopping that was the provider remembering to check.
func TestArmRegistrationTimeout(t *testing.T) {
	old := poolRegistrationTimeout
	poolRegistrationTimeout = time.Minute
	t.Cleanup(func() { poolRegistrationTimeout = old })

	stateChangedAt := time.Now().UTC().Add(-10 * time.Second)
	registeredAt := time.Now().UTC()
	waiting := func() *model.Pool {
		return &model.Pool{
			ID: "pool-1",
			ResourceLifecycle: model.ResourceLifecycle{
				State:          model.PoolStateRegistering,
				StateChangedAt: stateChangedAt,
			},
		}
	}

	t.Run("waiting pool is armed at its deadline", func(t *testing.T) {
		got := armRegistrationTimeout(waiting())
		want := stateChangedAt.Add(poolRegistrationTimeout)
		if !got.RequeueAt.Equal(want) {
			t.Fatalf("RequeueAt = %s, want %s", got.RequeueAt, want)
		}
	})

	t.Run("registered pool is not armed", func(t *testing.T) {
		pool := waiting()
		pool.RegisteredAt = &registeredAt
		if got := armRegistrationTimeout(pool); !got.RequeueAt.IsZero() {
			t.Fatalf("RequeueAt = %s, want none for a registered pool", got.RequeueAt)
		}
	})

	t.Run("pool that has been seen is not armed", func(t *testing.T) {
		pool := waiting()
		pool.LastSeenAt = &registeredAt
		if got := armRegistrationTimeout(pool); !got.RequeueAt.IsZero() {
			t.Fatalf("RequeueAt = %s, want none for a pool that has reported in", got.RequeueAt)
		}
	})

	t.Run("active pool is not armed", func(t *testing.T) {
		pool := waiting()
		pool.State = model.PoolStateActive
		if got := armRegistrationTimeout(pool); !got.RequeueAt.IsZero() {
			t.Fatalf("RequeueAt = %s, want none for an active pool", got.RequeueAt)
		}
	})

	t.Run("a disabled timeout arms nothing", func(t *testing.T) {
		poolRegistrationTimeout = 0
		t.Cleanup(func() { poolRegistrationTimeout = time.Minute })
		if got := armRegistrationTimeout(waiting()); !got.RequeueAt.IsZero() {
			t.Fatalf("RequeueAt = %s, want none when the timeout is disabled", got.RequeueAt)
		}
	})
}

// TestArmRegistrationTimeoutAgreesWithRegistrationExpired keeps the deadline and
// the check that consumes it from drifting apart: the moment the timer fires is
// exactly the moment registrationExpired starts returning true, so the wake it
// schedules always has work to do.
func TestArmRegistrationTimeoutAgreesWithRegistrationExpired(t *testing.T) {
	old := poolRegistrationTimeout
	poolRegistrationTimeout = time.Minute
	t.Cleanup(func() { poolRegistrationTimeout = old })

	pool := &model.Pool{
		ID: "pool-1",
		ResourceLifecycle: model.ResourceLifecycle{
			State:          model.PoolStateRegistering,
			StateChangedAt: time.Now().UTC().Add(-poolRegistrationTimeout).Add(-time.Second),
		},
	}
	if !(&PoolReconciler{}).registrationExpired(pool) {
		t.Fatal("registrationExpired = false for a pool past its armed deadline")
	}
	if got := armRegistrationTimeout(pool); got.RequeueAt.After(time.Now()) {
		t.Fatalf("RequeueAt = %s, want a deadline already passed", got.RequeueAt)
	}
}
