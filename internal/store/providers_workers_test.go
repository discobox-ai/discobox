package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/obot-platform/discobox/internal/model"
	"github.com/obot-platform/discobox/internal/store"
)

func TestWorkerRegisterStatusAndClaim(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "digitalocean", Name: "do"}
	if err := s.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	worker := &model.Worker{ID: "worker-1", TenantID: "tenant-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Identity: "worker-1"}
	bootstrap := "bootstrap-token"
	h := sha256.Sum256([]byte(bootstrap))
	if err := s.CreateWorkerWithBootstrapToken(ctx, worker, &model.WorkerBootstrapToken{TenantID: "tenant-1", WorkerID: worker.ID, TokenHash: h[:], ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("create worker bootstrap: %v", err)
	}
	authHash := sha256.Sum256([]byte("auth"))
	registered, err := s.RegisterWorker(ctx, "tenant-1", worker.ID, h[:], "public", "ed25519", authHash[:], time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}
	if !registered.Ready || !registered.Schedulable || registered.PublicKey != "public" {
		t.Fatalf("registered worker = %#v", registered)
	}
	if err := s.ValidateWorkerAuthToken(ctx, "tenant-1", worker.ID, authHash[:]); err != nil {
		t.Fatalf("validate auth token: %v", err)
	}
	updated, err := s.UpdateWorkerStatus(ctx, "tenant-1", worker.ID, true, true, true, 2, 4<<30, 10<<30, []byte(`{"pressure":"high"}`))
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if !updated.Degraded || updated.AvailableCPUVCPUs != 2 || updated.AvailableMemoryBytes != 4<<30 || updated.AvailableStorageBytes != 10<<30 || string(updated.Conditions) == "" {
		t.Fatalf("updated worker = %#v", updated)
	}
	claimed, err := s.ClaimWorker(ctx, sandboxForClaim("project-1", "provider-1", 1, 1<<30, 1<<30))
	if err != nil {
		t.Fatalf("claim worker: %v", err)
	}
	if claimed.ID != worker.ID {
		t.Fatalf("claimed %q, want %q", claimed.ID, worker.ID)
	}
}

func TestClaimWorkerSamplesTwoAndPicksBestResourceFit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	provider := &model.SandboxProviderInstance{ID: "provider-capacity", ProjectID: "project-1", Type: "digitalocean", Name: "do"}
	if err := s.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	now := time.Now().UTC()
	workers := []model.Worker{
		{ID: "worker-low", TenantID: "tenant-1", ProjectID: "project-1", ProviderInstanceID: provider.ID, Identity: "worker-low", Ready: true, Schedulable: true, AvailableCPUVCPUs: 2, AvailableMemoryBytes: 2 << 30, AvailableStorageBytes: 20 << 30, RegisteredAt: &now, LastSeenAt: &now, ResourceLifecycle: model.NewResourceLifecycle(model.WorkerCreateOperation, nil)},
		{ID: "worker-high", TenantID: "tenant-1", ProjectID: "project-1", ProviderInstanceID: provider.ID, Identity: "worker-high", Ready: true, Schedulable: true, AvailableCPUVCPUs: 4, AvailableMemoryBytes: 8 << 30, AvailableStorageBytes: 40 << 30, RegisteredAt: &now, LastSeenAt: &now, ResourceLifecycle: model.NewResourceLifecycle(model.WorkerCreateOperation, nil)},
	}
	for i := range workers {
		if err := s.CreateWorkerWithBootstrapToken(ctx, &workers[i], &model.WorkerBootstrapToken{TenantID: "tenant-1", WorkerID: workers[i].ID, TokenHash: []byte(workers[i].ID), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatalf("create worker %s: %v", workers[i].ID, err)
		}
	}

	claimed, err := s.ClaimWorker(ctx, sandboxForClaim("project-1", provider.ID, 1, 1<<30, 5<<30))
	if err != nil {
		t.Fatalf("claim worker: %v", err)
	}
	if claimed.ID != "worker-high" {
		t.Fatalf("claimed worker = %q, want worker-high", claimed.ID)
	}
}

func TestClaimWorkerRequiresResourceFit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	provider := &model.SandboxProviderInstance{ID: "provider-empty", ProjectID: "project-1", Type: "digitalocean", Name: "do"}
	if err := s.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	worker := &model.Worker{
		ID:                    "worker-empty",
		TenantID:              "tenant-1",
		ProjectID:             "project-1",
		ProviderInstanceID:    provider.ID,
		Identity:              "worker-empty",
		Ready:                 true,
		Schedulable:           true,
		AvailableCPUVCPUs:     1,
		AvailableMemoryBytes:  1 << 30,
		AvailableStorageBytes: 1 << 30,
		ResourceLifecycle:     model.NewResourceLifecycle(model.WorkerCreateOperation, nil),
	}
	if err := s.CreateWorkerWithBootstrapToken(ctx, worker, &model.WorkerBootstrapToken{TenantID: "tenant-1", WorkerID: worker.ID, TokenHash: []byte("empty"), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	_, err := s.ClaimWorker(ctx, sandboxForClaim("project-1", provider.ID, 2, 1<<30, 1<<30))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claim worker error = %v, want ErrNotFound", err)
	}
}

func sandboxForClaim(projectID, providerID string, cpuVCPUs float64, memoryBytes, storageBytes int64) *model.Sandbox {
	return &model.Sandbox{
		ProjectID:          projectID,
		ProviderInstanceID: &providerID,
		CPUVCPUs:           cpuVCPUs,
		MemoryBytes:        memoryBytes,
		StorageBytes:       storageBytes,
	}
}

func TestValidateWorkerAuthTokenRejectsInvalidExpiredAndRevoked(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, nil)
	provider := &model.SandboxProviderInstance{ID: "provider-auth", ProjectID: "project-1", Type: "digitalocean", Name: "do"}
	if err := db.Write.WithContext(ctx).Create(provider).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	worker := &model.Worker{ID: "worker-auth", TenantID: "tenant-1", ProjectID: "project-1", ProviderInstanceID: provider.ID, Identity: "worker-auth"}
	if err := db.Write.WithContext(ctx).Create(worker).Error; err != nil {
		t.Fatalf("create worker: %v", err)
	}

	validHash := sha256.Sum256([]byte("valid"))
	expiredHash := sha256.Sum256([]byte("expired"))
	revokedHash := sha256.Sum256([]byte("revoked"))
	invalidHash := sha256.Sum256([]byte("invalid"))
	now := time.Now().UTC()
	revokedAt := now
	if err := db.Write.WithContext(ctx).Create(&[]model.WorkerAuthToken{
		{TenantID: "tenant-1", WorkerID: worker.ID, TokenHash: validHash[:], IssuedAt: now, ExpiresAt: now.Add(time.Hour)},
		{TenantID: "tenant-1", WorkerID: worker.ID, TokenHash: expiredHash[:], IssuedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)},
		{TenantID: "tenant-1", WorkerID: worker.ID, TokenHash: revokedHash[:], IssuedAt: now, ExpiresAt: now.Add(time.Hour), RevokedAt: &revokedAt},
	}).Error; err != nil {
		t.Fatalf("create auth tokens: %v", err)
	}
	if err := s.ValidateWorkerAuthToken(ctx, "tenant-1", worker.ID, validHash[:]); err != nil {
		t.Fatalf("validate valid token: %v", err)
	}
	for name, hash := range map[string][]byte{"invalid": invalidHash[:], "expired": expiredHash[:], "revoked": revokedHash[:]} {
		if err := s.ValidateWorkerAuthToken(ctx, "tenant-1", worker.ID, hash); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s token error = %v, want ErrNotFound", name, err)
		}
	}
}
