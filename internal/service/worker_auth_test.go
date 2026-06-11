package service_test

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/obot-platform/disco2/internal/api"
	"github.com/obot-platform/disco2/internal/database"
	"github.com/obot-platform/disco2/internal/events"
	"github.com/obot-platform/disco2/internal/id"
	"github.com/obot-platform/disco2/internal/model"
	"github.com/obot-platform/disco2/internal/orchestration"
	"github.com/obot-platform/disco2/internal/service"
	"github.com/obot-platform/disco2/internal/store"
	"github.com/obot-platform/disco2/jobqueue"
)

func TestUpdateWorkerStatusRequiresValidBearerToken(t *testing.T) {
	ctx := context.Background()
	svc, appStore, db := newWorkerAuthService(t)
	workerID, token := registerTestWorker(t, ctx, svc, appStore)

	if _, err := svc.UpdateWorkerStatus(ctx, "Bearer "+token, api.UpdateWorkerStatusBody{TenantID: service.DefaultTenantID, WorkerID: workerID, Ready: true, Schedulable: true, AvailableCPUVCPUs: 1}); err != nil {
		t.Fatalf("update with valid token: %v", err)
	}
	if _, err := svc.UpdateWorkerStatus(ctx, "Bearer wrong", api.UpdateWorkerStatusBody{TenantID: service.DefaultTenantID, WorkerID: workerID, Ready: true, Schedulable: true, AvailableCPUVCPUs: 1}); err == nil {
		t.Fatal("expected invalid token to be rejected")
	}

	if err := db.Write.WithContext(ctx).Model(&model.WorkerAuthToken{}).Where("worker_id = ?", workerID).Update("expires_at", time.Now().UTC().Add(-time.Minute)).Error; err != nil {
		t.Fatalf("expire auth token: %v", err)
	}
	if _, err := svc.UpdateWorkerStatus(ctx, "Bearer "+token, api.UpdateWorkerStatusBody{TenantID: service.DefaultTenantID, WorkerID: workerID, Ready: true, Schedulable: true, AvailableCPUVCPUs: 1}); err == nil {
		t.Fatal("expected expired token to be rejected")
	}

	workerID, token = registerTestWorker(t, ctx, svc, appStore)
	revokedAt := time.Now().UTC()
	if err := db.Write.WithContext(ctx).Model(&model.WorkerAuthToken{}).Where("worker_id = ?", workerID).Update("revoked_at", revokedAt).Error; err != nil {
		t.Fatalf("revoke auth token: %v", err)
	}
	if _, err := svc.UpdateWorkerStatus(ctx, "Bearer "+token, api.UpdateWorkerStatusBody{TenantID: service.DefaultTenantID, WorkerID: workerID, Ready: true, Schedulable: true, AvailableCPUVCPUs: 1}); err == nil {
		t.Fatal("expected revoked token to be rejected")
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
	appStore := store.New(database.StaticResolver{DB: db}, store.WithPublisher(broker), store.WithDefaultTenantID(service.DefaultTenantID))
	queueConfig := jobqueue.QueueConfig{DefaultMaxAttempts: 3}
	ensureJob := func(ctx context.Context, txStore *store.Store, payload jobqueue.Payload) (*jobqueue.Job, bool, error) {
		return txStore.EnsureActiveJobForPayload(ctx, payload, queueConfig)
	}
	svc := service.New(appStore, orchestration.New(appStore, ensureJob, nil), broker)
	if err := svc.InitializeDefaults(ctx, service.DefaultTenantID, service.DefaultUserID); err != nil {
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
	worker := &model.Worker{ID: id.NewString(), TenantID: service.DefaultTenantID, ProjectID: service.DefaultProjectID, ProviderInstanceID: "provider-auth"}
	bootstrap := "bootstrap-" + time.Now().String()
	h := sha256.Sum256([]byte(bootstrap))
	if err := appStore.CreateWorkerWithBootstrapToken(ctx, worker, &model.WorkerBootstrapToken{TenantID: service.DefaultTenantID, WorkerID: worker.ID, TokenHash: h[:], ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("create worker bootstrap: %v", err)
	}
	resp, err := svc.RegisterWorker(ctx, api.RegisterWorkerBody{TenantID: service.DefaultTenantID, WorkerID: worker.ID, BootstrapToken: bootstrap, PublicKey: "public"})
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}
	return worker.ID, resp.AuthToken
}
