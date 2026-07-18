package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/store"
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
	if !registered.Ready || !registered.Schedulable || registered.PublicKey != "public" {
		t.Fatalf("registered pool = %#v", registered)
	}
	updated, err := s.UpdatePoolStatus(ctx, "pool-1", true, true, true, 2, 4<<30, 10<<30, []byte(`{"pressure":"high"}`))
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if !updated.Degraded || updated.AvailableCPUVCPUs != 2 || updated.AvailableMemoryBytes != 4<<30 || updated.AvailableStorageBytes != 10<<30 || string(updated.Conditions) == "" {
		t.Fatalf("updated pool = %#v", updated)
	}
	pool, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", "pool-1", 1, 1<<30, 1<<30))
	if err != nil {
		t.Fatalf("schedulable pool: %v", err)
	}
	if pool.ID != "pool-1" {
		t.Fatalf("schedulable pool = %q, want pool-1", pool.ID)
	}
}

func TestSchedulablePoolForSandboxRequiresResourceFit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	if _, err := s.UpdatePoolStatus(ctx, "pool-1", true, true, false, 1, 1<<30, 1<<30, nil); err != nil {
		t.Fatalf("update status: %v", err)
	}

	if _, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", "pool-1", 2, 1<<30, 1<<30)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("schedulable pool error = %v, want ErrNotFound", err)
	}
	if pool, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", "pool-1", 1, 1<<30, 1<<30)); err != nil || pool.ID != "pool-1" {
		t.Fatalf("schedulable pool = %v err=%v, want pool-1", pool, err)
	}
}

func TestSchedulablePoolForSandboxRequiresReadiness(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")

	// A pool that never reported ready cannot accept placement.
	if _, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", "pool-1", 1, 0, 0)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("schedulable pool error = %v, want ErrNotFound", err)
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

func sandboxForClaim(projectID, poolID string, cpuVCPUs float64, memoryBytes, storageBytes int64) *model.Sandbox {
	return &model.Sandbox{
		ProjectID:    projectID,
		PoolID:       poolID,
		CPUVCPUs:     cpuVCPUs,
		MemoryBytes:  memoryBytes,
		StorageBytes: storageBytes,
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
