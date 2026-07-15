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

func TestWorkerRegisterStatusAndClaim(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "digitalocean", Name: "do"}
	if err := s.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	worker := &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Identity: "worker-1"}
	bootstrap := "bootstrap-token"
	h := sha256.Sum256([]byte(bootstrap))
	if err := s.CreateWorkerWithBootstrapToken(ctx, worker, &model.WorkerBootstrapToken{WorkerID: worker.ID, TokenHash: h[:], ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("create worker bootstrap: %v", err)
	}
	registered, err := s.RegisterWorker(ctx, worker.ID, h[:], "public", "ed25519")
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}
	if !registered.Ready || !registered.Schedulable || registered.PublicKey != "public" {
		t.Fatalf("registered worker = %#v", registered)
	}
	updated, err := s.UpdateWorkerStatus(ctx, worker.ID, true, true, true, 2, 4<<30, 10<<30, []byte(`{"pressure":"high"}`))
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

func TestListSandboxProviderInstancesWithWorkersPreloadsWorkers(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "Docker"}
	if err := s.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	otherProvider := &model.SandboxProviderInstance{ID: "provider-2", ProjectID: "project-1", Type: "docker", Name: "Other Docker"}
	if err := s.CreateSandboxProviderInstance(ctx, otherProvider); err != nil {
		t.Fatalf("create other provider: %v", err)
	}
	if err := s.UpsertProject(ctx, &model.Project{ID: "project-2", OwnerUserID: "user-1", Name: "Other Project", Slug: "other-project"}); err != nil {
		t.Fatalf("create outside project: %v", err)
	}
	outsideProjectProvider := &model.SandboxProviderInstance{ID: "provider-3", ProjectID: "project-2", Type: "docker", Name: "Outside Docker"}
	if err := s.CreateSandboxProviderInstance(ctx, outsideProjectProvider); err != nil {
		t.Fatalf("create outside provider: %v", err)
	}
	for _, worker := range []model.Worker{
		{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: provider.ID, Identity: "worker-1"},
		{ID: "worker-2", ProjectID: "project-1", ProviderInstanceID: otherProvider.ID, Identity: "worker-2"},
		{ID: "worker-3", ProjectID: "project-2", ProviderInstanceID: outsideProjectProvider.ID, Identity: "worker-3"},
	} {
		worker := worker
		if err := s.CreateWorker(ctx, &worker); err != nil {
			t.Fatalf("create worker %s: %v", worker.ID, err)
		}
	}

	providers, err := s.ListSandboxProviderInstancesWithWorkers(ctx, "project-1")
	if err != nil {
		t.Fatalf("list providers with workers: %v", err)
	}
	if len(providers) != 2 {
		t.Fatalf("providers len = %d, want 2", len(providers))
	}
	for _, provider := range providers {
		if len(provider.Workers) != 1 {
			t.Fatalf("provider %s workers = %#v, want one worker", provider.ID, provider.Workers)
		}
		if provider.Workers[0].ProviderInstanceID != provider.ID {
			t.Fatalf("provider %s loaded worker for provider %s", provider.ID, provider.Workers[0].ProviderInstanceID)
		}
		if provider.Workers[0].ProjectID != "project-1" {
			t.Fatalf("provider %s loaded worker from project %s", provider.ID, provider.Workers[0].ProjectID)
		}
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
		{ID: "worker-low", ProjectID: "project-1", ProviderInstanceID: provider.ID, Identity: "worker-low", Ready: true, Schedulable: true, AvailableCPUVCPUs: 2, AvailableMemoryBytes: 2 << 30, AvailableStorageBytes: 20 << 30, RegisteredAt: &now, LastSeenAt: &now, ResourceLifecycle: model.NewResourceLifecycle(model.WorkerCreateOperation)},
		{ID: "worker-high", ProjectID: "project-1", ProviderInstanceID: provider.ID, Identity: "worker-high", Ready: true, Schedulable: true, AvailableCPUVCPUs: 4, AvailableMemoryBytes: 8 << 30, AvailableStorageBytes: 40 << 30, RegisteredAt: &now, LastSeenAt: &now, ResourceLifecycle: model.NewResourceLifecycle(model.WorkerCreateOperation)},
	}
	for i := range workers {
		if err := s.CreateWorkerWithBootstrapToken(ctx, &workers[i], &model.WorkerBootstrapToken{WorkerID: workers[i].ID, TokenHash: []byte(workers[i].ID), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
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
		ProjectID:             "project-1",
		ProviderInstanceID:    provider.ID,
		Identity:              "worker-empty",
		Ready:                 true,
		Schedulable:           true,
		AvailableCPUVCPUs:     1,
		AvailableMemoryBytes:  1 << 30,
		AvailableStorageBytes: 1 << 30,
		ResourceLifecycle:     model.NewResourceLifecycle(model.WorkerCreateOperation),
	}
	if err := s.CreateWorkerWithBootstrapToken(ctx, worker, &model.WorkerBootstrapToken{WorkerID: worker.ID, TokenHash: []byte("empty"), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
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

func sandboxForClaim(projectID, providerID string, cpuVCPUs float64, memoryBytes, storageBytes int64) *model.Sandbox {
	return &model.Sandbox{
		ProjectID:          projectID,
		ProviderInstanceID: &providerID,
		CPUVCPUs:           cpuVCPUs,
		MemoryBytes:        memoryBytes,
		StorageBytes:       storageBytes,
	}
}

// TestPurgeSpentWorkerBootstrapTokens pins the bound on the bootstrap token
// table: spent tokens (expired, used, or revoked) are collected, and a token
// that can still be redeemed is left alone.
func TestPurgeSpentWorkerBootstrapTokens(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "docker"}
	if err := s.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	worker := &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1", Identity: "worker-1"}
	live := sha256.Sum256([]byte("live-token"))
	if err := s.CreateWorkerWithBootstrapToken(ctx, worker, &model.WorkerBootstrapToken{WorkerID: worker.ID, TokenHash: live[:], ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("create worker bootstrap: %v", err)
	}
	expired := sha256.Sum256([]byte("expired-token"))
	if err := s.CreateWorkerBootstrapToken(ctx, &model.WorkerBootstrapToken{WorkerID: worker.ID, TokenHash: expired[:], ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("create expired bootstrap token: %v", err)
	}

	purged, err := s.PurgeSpentWorkerBootstrapTokens(ctx, time.Now())
	if err != nil {
		t.Fatalf("purge spent bootstrap tokens: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 spent token", purged)
	}

	// The live token still redeems: purging must not touch it.
	if _, err := s.RegisterWorker(ctx, worker.ID, live[:], "public", "ed25519"); err != nil {
		t.Fatalf("register worker with surviving live token: %v", err)
	}
}
