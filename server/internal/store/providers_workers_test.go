package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/server/internal/store"
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
	claimed, err := s.FindSchedulableWorker(ctx, sandboxForClaim("project-1", "provider-1", 1, 1<<30, 1<<30))
	if err != nil {
		t.Fatalf("find schedulable worker: %v", err)
	}
	if claimed.ID != worker.ID {
		t.Fatalf("claimed %q, want %q", claimed.ID, worker.ID)
	}
}

func TestFindSchedulableWorkerSamplesTwoAndPicksBestResourceFit(t *testing.T) {
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

	claimed, err := s.FindSchedulableWorker(ctx, sandboxForClaim("project-1", provider.ID, 1, 1<<30, 5<<30))
	if err != nil {
		t.Fatalf("find schedulable worker: %v", err)
	}
	if claimed.ID != "worker-high" {
		t.Fatalf("schedulable worker = %q, want worker-high", claimed.ID)
	}
}

func TestFindSchedulableWorkerRequiresResourceFit(t *testing.T) {
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

	_, err := s.FindSchedulableWorker(ctx, sandboxForClaim("project-1", provider.ID, 2, 1<<30, 1<<30))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("find schedulable worker error = %v, want ErrNotFound", err)
	}
}

func TestWorkerGenerationOptions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	provider := &model.SandboxProviderInstance{ID: "provider-generation", ProjectID: "project-1", Type: "digitalocean", Name: "do"}
	if err := s.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	worker := &model.Worker{
		ID:                 "worker-generation",
		TenantID:           "tenant-1",
		ProjectID:          "project-1",
		ProviderInstanceID: provider.ID,
		Identity:           "worker-generation",
	}
	if err := s.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	got, err := s.GetWorker(ctx, worker.ID, store.WithWorkerGeneration(worker.Generation))
	if err != nil {
		t.Fatalf("get matching generation: %v", err)
	}
	if got.ID != worker.ID {
		t.Fatalf("worker id = %q, want %q", got.ID, worker.ID)
	}

	if _, err := s.GetWorker(ctx, worker.ID, store.WithWorkerGeneration(worker.Generation+1)); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("get stale generation error = %v, want ErrGenerationConflict", err)
	}

	worker.Identity = "worker-generation-renamed"
	if err := s.UpdateWorker(ctx, worker, store.WithWorkerGeneration(worker.Generation)); err != nil {
		t.Fatalf("update matching generation: %v", err)
	}

	worker.Identity = "worker-generation-stale"
	if err := s.UpdateWorker(ctx, worker, store.WithWorkerGeneration(worker.Generation+1)); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("update stale generation error = %v, want ErrGenerationConflict", err)
	}
}

func TestMarkWorkerFailedForJobRequiresCurrentJobAndGeneration(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	provider := &model.SandboxProviderInstance{ID: "provider-worker-job", ProjectID: "project-1", Type: "digitalocean", Name: "do"}
	if err := s.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	currentJobID := "job-current"
	worker := &model.Worker{
		ID:                 "worker-job",
		TenantID:           "tenant-1",
		ProjectID:          "project-1",
		ProviderInstanceID: provider.ID,
		Identity:           "worker-job",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateActive,
			Phase:               model.WorkerPhaseLaunching,
			LastOperationStatus: model.OperationStatusRunning,
			LastJobID:           &currentJobID,
		},
	}
	if err := s.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	updated, err := s.MarkWorkerFailedForJob(ctx, worker.ID, worker.Generation, "job-stale", "stale failed")
	if err != nil {
		t.Fatalf("mark stale job failed: %v", err)
	}
	if updated {
		t.Fatal("stale job updated worker")
	}
	got, err := s.GetWorker(ctx, worker.ID)
	if err != nil {
		t.Fatalf("get worker after stale job: %v", err)
	}
	if got.Phase != model.WorkerPhaseLaunching || got.LastOperationStatus != model.OperationStatusRunning || got.ErrorMessage != nil {
		t.Fatalf("worker after stale repair = phase %q status %q error %v", got.Phase, got.LastOperationStatus, got.ErrorMessage)
	}

	updated, err = s.MarkWorkerFailedForJob(ctx, worker.ID, worker.Generation+1, currentJobID, "wrong generation")
	if err != nil {
		t.Fatalf("mark stale generation failed: %v", err)
	}
	if updated {
		t.Fatal("stale generation updated worker")
	}

	updated, err = s.MarkWorkerFailedForJob(ctx, worker.ID, worker.Generation, currentJobID, "image not found")
	if err != nil {
		t.Fatalf("mark current job failed: %v", err)
	}
	if !updated {
		t.Fatal("current job did not update worker")
	}
	got, err = s.GetWorker(ctx, worker.ID)
	if err != nil {
		t.Fatalf("get worker after current job: %v", err)
	}
	if got.Phase != model.WorkerPhaseFailed || got.LastOperationStatus != model.OperationStatusFailed {
		t.Fatalf("worker after current repair = phase %q status %q, want failed/failed", got.Phase, got.LastOperationStatus)
	}
	if got.ErrorMessage == nil || *got.ErrorMessage != "image not found" {
		t.Fatalf("worker error = %v, want image not found", got.ErrorMessage)
	}
}

func TestMarkWorkerRegistrationExpiredRequiresCurrentRegisteringWorker(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	provider := &model.SandboxProviderInstance{ID: "provider-registration", ProjectID: "project-1", Type: "digitalocean", Name: "do"}
	if err := s.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	now := time.Now().UTC()
	worker := &model.Worker{
		ID:                 "worker-registration",
		TenantID:           "tenant-1",
		ProjectID:          "project-1",
		ProviderInstanceID: provider.ID,
		Identity:           "worker-registration",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateActive,
			Phase:               model.WorkerPhaseRegistering,
			LastOperationStatus: model.OperationStatusSuccess,
		},
		UpdatedAt: now.Add(-2 * time.Minute),
	}
	if err := s.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	updated, err := s.MarkWorkerRegistrationExpired(ctx, worker.ID, worker.Generation+1, now.Add(-time.Minute), "wrong generation")
	if err != nil {
		t.Fatalf("mark stale generation expired: %v", err)
	}
	if updated {
		t.Fatal("stale generation updated worker")
	}

	updated, err = s.MarkWorkerRegistrationExpired(ctx, worker.ID, worker.Generation, now.Add(-3*time.Minute), "not expired")
	if err != nil {
		t.Fatalf("mark not expired: %v", err)
	}
	if updated {
		t.Fatal("worker newer than cutoff was updated")
	}

	updated, err = s.MarkWorkerRegistrationExpired(ctx, worker.ID, worker.Generation, now.Add(-time.Minute), "registration timed out")
	if err != nil {
		t.Fatalf("mark registration expired: %v", err)
	}
	if !updated {
		t.Fatal("expired registration did not update worker")
	}
	got, err := s.GetWorker(ctx, worker.ID)
	if err != nil {
		t.Fatalf("get worker after registration expiry: %v", err)
	}
	if got.Phase != model.WorkerPhaseFailed || got.LastOperationStatus != model.OperationStatusFailed {
		t.Fatalf("worker after expiry = phase %q status %q, want failed/failed", got.Phase, got.LastOperationStatus)
	}
	if got.ErrorMessage == nil || *got.ErrorMessage != "registration timed out" {
		t.Fatalf("worker error = %v, want registration timed out", got.ErrorMessage)
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
