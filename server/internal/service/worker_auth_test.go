package service_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/obot-platform/discobox/id"
	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/api"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/events"
	"github.com/obot-platform/discobox/server/internal/service"
	"github.com/obot-platform/discobox/server/internal/store"
)

func TestUpdateWorkerStatusRequiresValidBearerToken(t *testing.T) {
	ctx := context.Background()
	svc, appStore, db := newWorkerAuthService(t)
	workerID, token := registerTestWorker(t, ctx, svc, appStore)

	if _, err := svc.UpdateWorkerStatus(ctx, "Bearer "+token, api.UpdateWorkerStatusBody{WorkerID: workerID, Ready: true, Schedulable: true, AvailableCPUVCPUs: 1}); err != nil {
		t.Fatalf("update with valid token: %v", err)
	}
	if _, err := svc.UpdateWorkerStatus(ctx, "Bearer wrong", api.UpdateWorkerStatusBody{WorkerID: workerID, Ready: true, Schedulable: true, AvailableCPUVCPUs: 1}); err == nil {
		t.Fatal("expected invalid token to be rejected")
	}

	if err := db.Write.WithContext(ctx).Model(&model.WorkerAuthToken{}).Where("worker_id = ?", workerID).Update("expires_at", time.Now().UTC().Add(-time.Minute)).Error; err != nil {
		t.Fatalf("expire auth token: %v", err)
	}
	if _, err := svc.UpdateWorkerStatus(ctx, "Bearer "+token, api.UpdateWorkerStatusBody{WorkerID: workerID, Ready: true, Schedulable: true, AvailableCPUVCPUs: 1}); err == nil {
		t.Fatal("expected expired token to be rejected")
	}

	workerID, token = registerTestWorker(t, ctx, svc, appStore)
	revokedAt := time.Now().UTC()
	if err := db.Write.WithContext(ctx).Model(&model.WorkerAuthToken{}).Where("worker_id = ?", workerID).Update("revoked_at", revokedAt).Error; err != nil {
		t.Fatalf("revoke auth token: %v", err)
	}
	if _, err := svc.UpdateWorkerStatus(ctx, "Bearer "+token, api.UpdateWorkerStatusBody{WorkerID: workerID, Ready: true, Schedulable: true, AvailableCPUVCPUs: 1}); err == nil {
		t.Fatal("expected revoked token to be rejected")
	}
}

func TestGetSandboxProviderInstanceIncludesWorkerStatus(t *testing.T) {
	ctx := context.Background()
	svc, appStore, db := newWorkerAuthService(t)
	workerID, token := registerTestWorker(t, ctx, svc, appStore)
	if _, err := svc.UpdateWorkerStatus(ctx, "Bearer "+token, api.UpdateWorkerStatusBody{
		WorkerID:              workerID,
		Ready:                 true,
		Schedulable:           true,
		AvailableCPUVCPUs:     2,
		AvailableMemoryBytes:  1024,
		AvailableStorageBytes: 2048,
	}); err != nil {
		t.Fatalf("update worker status: %v", err)
	}
	runtimeState := json.RawMessage(`{"instanceId":"container-1","worker":{"token":"bootstrap-secret","workerId":"worker-1"}}`)
	if err := db.Write.WithContext(ctx).Model(&model.Worker{}).Where("id = ?", workerID).Update("runtime_state", runtimeState).Error; err != nil {
		t.Fatalf("update runtime state: %v", err)
	}

	provider, err := svc.GetSandboxProviderInstance(ctx, service.DefaultProjectID, "provider-auth")
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if provider.Status == nil {
		t.Fatal("provider status is nil")
	}
	if provider.Status.WorkerCount != 1 || provider.Status.ReadyWorkers != 1 || provider.Status.SchedulableWorkers != 1 {
		t.Fatalf("provider status counts = %#v, want one ready schedulable worker", provider.Status)
	}
	if len(provider.Status.Workers) != 1 {
		t.Fatalf("provider status workers = %d, want 1", len(provider.Status.Workers))
	}
	workerStatus := provider.Status.Workers[0]
	if workerStatus.ID != workerID || workerStatus.AvailableCPUVCPUs != 2 {
		t.Fatalf("worker status = %#v, want worker %s with CPU 2", workerStatus, workerID)
	}
	if workerStatus.RuntimeID != "container-1" {
		t.Fatalf("worker runtime ID = %q, want container-1", workerStatus.RuntimeID)
	}
	if len(provider.Workers) != 0 {
		t.Fatalf("provider workers len = %d, want 0 to avoid exposing raw worker state", len(provider.Workers))
	}
	response, err := json.Marshal(provider)
	if err != nil {
		t.Fatalf("marshal provider: %v", err)
	}
	if strings.Contains(string(response), "bootstrap-secret") || strings.Contains(string(response), "runtimeState") {
		t.Fatalf("provider response exposes bootstrap secret: %s", response)
	}
}

func newWorkerAuthService(t *testing.T) (*service.Service, *store.Store, *database.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	broker := events.NewBroker()
	appStore := store.New(db.Write, db.Read, store.WithPublisher(broker))
	queueConfig := orchestration.QueueConfig{DefaultMaxAttempts: 3}
	svc := service.New(appStore, queueConfig, nil, broker)
	if err := svc.InitializeDefaults(ctx, service.DefaultUserID); err != nil {
		t.Fatalf("init defaults: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "provider-auth", ProjectID: service.DefaultProjectID, Type: "digitalocean", Name: "do"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return svc, appStore, db
}

func registerTestWorker(t *testing.T, ctx context.Context, svc *service.Service, appStore *store.Store) (string, string) {
	t.Helper()
	worker := &model.Worker{ID: id.NewString(), ProjectID: service.DefaultProjectID, ProviderInstanceID: "provider-auth"}
	bootstrap := "bootstrap-" + time.Now().String()
	h := sha256.Sum256([]byte(bootstrap))
	if err := appStore.CreateWorkerWithBootstrapToken(ctx, worker, &model.WorkerBootstrapToken{WorkerID: worker.ID, TokenHash: h[:], ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("create worker bootstrap: %v", err)
	}
	resp, err := svc.RegisterWorker(ctx, api.RegisterWorkerBody{WorkerID: worker.ID, BootstrapToken: bootstrap, PublicKey: "public"})
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}
	return worker.ID, resp.AuthToken
}
