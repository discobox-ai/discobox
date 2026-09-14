package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func TestPoolRegisterStatusAndSchedulableGate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")

	bootstrap := "bootstrap-token"
	h := sha256.Sum256([]byte(bootstrap))
	if err := s.CreatePoolBootstrapToken(ctx, &model.PoolBootstrapToken{PoolID: "pool-1", TokenHash: h[:], ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("create pool bootstrap: %v", err)
	}
	registered, err := s.RegisterPool(ctx, "pool-1", h[:], "public", "ed25519")
	if err != nil {
		t.Fatalf("register pool: %v", err)
	}
	// Registration establishes identity and liveness only: health arrives on
	// the agent's own heartbeat, and State/ObservedGeneration are the
	// reconciler's to write.
	if registered.PublicKey != "public" || registered.RegisteredAt == nil || registered.LastSeenAt == nil {
		t.Fatalf("registered pool = %#v", registered)
	}
	if registered.Ready || registered.Schedulable {
		t.Fatalf("registration reported health: %#v", registered)
	}
	if registered.State == model.PoolStateActive {
		t.Fatal("registration wrote the reconciler's state")
	}
	updated, err := s.UpdatePoolStatus(ctx, "pool-1", true, true, true, 2, 4<<30, 10<<30, []byte(`{"pressure":"high"}`))
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if !updated.Degraded || updated.AvailableCPUVCPUs != 2 || updated.AvailableMemoryBytes != 4<<30 || updated.AvailableStorageBytes != 10<<30 || string(updated.Conditions) == "" {
		t.Fatalf("updated pool = %#v", updated)
	}
	// A ready heartbeat cannot admit work while startup is still preloading.
	if _, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", "pool-1")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("startup admitted work: %v", err)
	}
	updated.SetState(model.PoolStateActive)
	if err := s.UpdatePoolWithGeneration(ctx, updated, updated.Generation); err != nil {
		t.Fatal(err)
	}
	pool, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", "pool-1"))
	if err != nil {
		t.Fatalf("schedulable pool: %v", err)
	}
	if pool.ID != "pool-1" {
		t.Fatalf("schedulable pool = %q, want pool-1", pool.ID)
	}
}

// TestUpdatePoolStatusLeavesReconcilerVerdictAlone pins the ownership split: a
// heartbeat reports health, and health alone. Writing State=active whenever
// the agent reported ready would repaint a recorded `offline` every few
// seconds — so a pool whose reconcile kept failing would read as active with a
// stale error hanging off it.
func TestUpdatePoolStatusLeavesReconcilerVerdictAlone(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")

	pool, err := s.GetPool(ctx, "project-1", "pool-1")
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	pool.RecordFailure(model.PoolStateOffline, "runtime did not converge")
	if err := s.UpdatePoolWithGeneration(ctx, pool, pool.Generation); err != nil {
		t.Fatalf("record failure: %v", err)
	}

	updated, err := s.UpdatePoolStatus(ctx, "pool-1", true, true, false, 1, 1<<30, 1<<30, nil)
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if !updated.Ready || !updated.Schedulable || updated.LastSeenAt == nil {
		t.Fatalf("heartbeat did not record health: %#v", updated)
	}
	if updated.State != model.PoolStateOffline {
		t.Fatalf("state = %q, want the reconciler's %q to survive the heartbeat", updated.State, model.PoolStateOffline)
	}
	if updated.ErrorMessage == nil {
		t.Fatal("heartbeat dropped the recorded error; only a successful reconcile clears it")
	}
}

// TestSchedulablePoolForSandboxIgnoresCapacity pins that placement is never
// refused for low reported CPU/memory/storage (docs/adr/0029): sandboxes
// share their pool's resources with no per-sandbox reservation, so a pool
// reporting almost no available capacity is still schedulable as long as it
// is ready and schedulable.
func TestSchedulablePoolForSandboxIgnoresCapacity(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	if _, err := s.UpdatePoolStatus(ctx, "pool-1", true, true, false, 0, 0, 0, nil); err != nil {
		t.Fatalf("update status: %v", err)
	}

	pool, err := s.GetPool(ctx, "project-1", "pool-1")
	if err != nil {
		t.Fatal(err)
	}
	pool.SetState(model.PoolStateActive)
	if err := s.UpdatePoolWithGeneration(ctx, pool, pool.Generation); err != nil {
		t.Fatal(err)
	}
	if pool, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", "pool-1")); err != nil || pool.ID != "pool-1" {
		t.Fatalf("schedulable pool = %v err=%v, want pool-1", pool, err)
	}
}

func TestSchedulablePoolForSandboxRequiresReadiness(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")

	// A pool that never reported ready cannot accept placement.
	if _, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", "pool-1")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("schedulable pool error = %v, want ErrNotFound", err)
	}
}

// A fresh heartbeat recovers placement even if the runtime reconciler is still
// blocked and has not refreshed its older offline verdict.
func TestSchedulablePoolForSandboxUsesFreshHealthRatherThanLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	pool, err := s.GetPool(ctx, "project-1", "pool-1")
	if err != nil {
		t.Fatal(err)
	}
	pool.RecordFailure(model.PoolStateOffline, "pool agent has not reported")
	if err := s.UpdatePoolWithGeneration(ctx, pool, pool.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdatePoolStatus(ctx, "pool-1", true, true, false, 1, 1<<30, 1<<30, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", "pool-1")); err != nil {
		t.Fatalf("fresh heartbeat did not recover placement: %v", err)
	}
}

func TestPoolStartupInvalidatesHealthAndPreservesIdentity(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	pool, err := s.GetPool(ctx, "project-1", "pool-1")
	if err != nil {
		t.Fatal(err)
	}
	token := sha256.Sum256([]byte("startup-health-token"))
	if err := s.CreatePoolBootstrapToken(ctx, &model.PoolBootstrapToken{PoolID: pool.ID, TokenHash: token[:], ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	pool, err = s.RegisterPool(ctx, pool.ID, token[:], "durable-key", "ed25519")
	if err != nil {
		t.Fatal(err)
	}
	pool.SetState(model.PoolStateActive)
	if err := s.UpdatePoolWithGeneration(ctx, pool, pool.Generation); err != nil {
		t.Fatal(err)
	}
	reported, err := s.UpdatePoolStatus(ctx, pool.ID, true, true, false, 1, 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A restart follows the report it invalidates. Windows' clock can return
	// the same instant for both, which reads as a report from after the restart.
	for !time.Now().After(*reported.StatusReportedAt) {
		time.Sleep(time.Millisecond)
	}
	if err := s.BeginPoolHealthChecks(ctx); err != nil {
		t.Fatal(err)
	}
	pool, err = s.GetPool(ctx, "project-1", "pool-1")
	if err != nil {
		t.Fatal(err)
	}
	if pool.Health(time.Now()) != model.PoolHealthUnknown || pool.PublicKey != "durable-key" || pool.RegisteredAt == nil || pool.State != model.PoolStateActive {
		t.Fatalf("restart changed identity/lifecycle or kept health: %+v", pool)
	}
	if _, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", pool.ID)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("startup placement = %v, want wait", err)
	}
	if _, err := s.UpdatePoolStatus(ctx, pool.ID, false, false, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	// A reconcile holding a pre-heartbeat snapshot must not erase freshness.
	if err := s.UpdatePoolWithGeneration(ctx, pool, pool.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", pool.ID)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale reconcile overwrote a negative heartbeat: %v", err)
	}
	if _, err := s.UpdatePoolStatus(ctx, pool.ID, true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", pool.ID)); err != nil {
		t.Fatalf("fresh heartbeat did not reopen placement: %v", err)
	}
}

func TestPoolGenerationOptions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")

	pool, err := s.GetPoolByID(ctx, "pool-1", store.WithPoolGeneration(0))
	if err != nil {
		t.Fatalf("get matching generation: %v", err)
	}
	if _, err := s.GetPoolByID(ctx, "pool-1", store.WithPoolGeneration(pool.Generation+1)); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("get stale generation error = %v, want ErrGenerationConflict", err)
	}

	pool.Name = "pool-renamed"
	if err := s.UpdatePoolWithGeneration(ctx, pool, pool.Generation); err != nil {
		t.Fatalf("update matching generation: %v", err)
	}
	pool.Name = "pool-stale"
	if err := s.UpdatePoolWithGeneration(ctx, pool, pool.Generation+1); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("update stale generation error = %v, want ErrGenerationConflict", err)
	}
}

func sandboxForClaim(projectID, poolID string) *model.Sandbox {
	return &model.Sandbox{
		ProjectID: projectID,
		PoolID:    poolID,
	}
}

// TestPurgeSpentPoolBootstrapTokens pins the bound on the bootstrap token
// table: spent tokens (expired, used, or revoked) are collected, and a token
// that can still be redeemed is left alone.
func TestPurgeSpentPoolBootstrapTokens(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")

	live := sha256.Sum256([]byte("live-token"))
	if err := s.CreatePoolBootstrapToken(ctx, &model.PoolBootstrapToken{PoolID: "pool-1", TokenHash: live[:], ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("create live bootstrap token: %v", err)
	}
	expired := sha256.Sum256([]byte("expired-token"))
	if err := s.CreatePoolBootstrapToken(ctx, &model.PoolBootstrapToken{PoolID: "pool-1", TokenHash: expired[:], ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("create expired bootstrap token: %v", err)
	}

	purged, err := s.PurgeSpentPoolBootstrapTokens(ctx, time.Now())
	if err != nil {
		t.Fatalf("purge spent bootstrap tokens: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 spent token", purged)
	}

	// The live token still redeems: purging must not touch it.
	if _, err := s.RegisterPool(ctx, "pool-1", live[:], "public", "ed25519"); err != nil {
		t.Fatalf("register pool with surviving live token: %v", err)
	}
}

func TestSchedulablePoolKeepsPreloadGateDespiteFreshHeartbeat(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	if _, err := s.UpdatePoolStatus(ctx, "pool-1", true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	pool, err := s.GetPool(ctx, "project-1", "pool-1")
	if err != nil {
		t.Fatal(err)
	}
	pool.SetState(model.PoolStateRegistering)
	if err := s.UpdatePoolWithGeneration(ctx, pool, pool.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", "pool-1")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("placement during preload = %v, want ErrNotFound", err)
	}
}
