package service_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/obot-platform/discobox/id"
	"github.com/obot-platform/discobox/server/internal/auth"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/events"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/service"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
	"github.com/obot-platform/discobox/worker-agent/workerauth"
)

func TestUpdateWorkerStatusRequiresValidWorkerAssertion(t *testing.T) {
	ctx := context.Background()
	svc, appStore, db := newWorkerAuthService(t)
	workerID, privateKey := registerTestWorker(ctx, t, svc, appStore)
	token := signTestWorkerAssertion(t, service.DefaultProjectID, workerID, privateKey)

	if _, err := updateTestWorkerStatus(ctx, svc, appStore, "Bearer "+token, workerID, services.UpdateWorkerStatusBody{Ready: true, Schedulable: true, AvailableCpuVcpus: 1}); err != nil {
		t.Fatalf("update with valid assertion: %v", err)
	}
	if _, err := updateTestWorkerStatus(ctx, svc, appStore, "Bearer wrong", workerID, services.UpdateWorkerStatusBody{Ready: true, Schedulable: true, AvailableCpuVcpus: 1}); err == nil {
		t.Fatal("expected invalid assertion to be rejected")
	}

	revokedAt := time.Now().UTC()
	if err := db.Write.WithContext(ctx).Model(&model.Worker{}).Where("id = ?", workerID).Update("revoked_at", revokedAt).Error; err != nil {
		t.Fatalf("revoke worker: %v", err)
	}
	if _, err := authenticateTestWorker(ctx, appStore, "Bearer "+token, workerID); err == nil {
		t.Fatal("expected revoked worker assertion to be rejected")
	}
	otherWorkerID, otherPrivateKey := registerTestWorker(ctx, t, svc, appStore)
	otherToken := signTestWorkerAssertion(t, service.DefaultProjectID, otherWorkerID, otherPrivateKey)
	workerCtx, err := authenticateTestWorker(ctx, appStore, "Bearer "+otherToken, otherWorkerID)
	if err != nil {
		t.Fatalf("authenticate other worker assertion: %v", err)
	}
	principal, ok := auth.PrincipalFromContext(workerCtx)
	if !ok || principal.WorkerID != otherWorkerID {
		t.Fatalf("authenticated principal = %#v, ok %v, want worker %s", principal, ok, otherWorkerID)
	}
	if _, err := svc.UpdateWorkerStatus(workerCtx, workerID, services.UpdateWorkerStatusBody{Ready: true, Schedulable: true, AvailableCpuVcpus: 1}); err == nil {
		t.Fatalf("expected worker %s principal to be forbidden for worker %s URL", otherWorkerID, workerID)
	}
}

func TestGetSandboxProviderInstanceIncludesWorkerStatus(t *testing.T) {
	ctx := context.Background()
	svc, appStore, db := newWorkerAuthService(t)
	workerID, privateKey := registerTestWorker(ctx, t, svc, appStore)
	token := signTestWorkerAssertion(t, service.DefaultProjectID, workerID, privateKey)
	if _, err := updateTestWorkerStatus(ctx, svc, appStore, "Bearer "+token, workerID, services.UpdateWorkerStatusBody{
		Ready:                 true,
		Schedulable:           true,
		AvailableCpuVcpus:     2,
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
	svc := service.New(appStore, nil, service.JobManagerOptions{}, broker)
	if err := svc.InitializeDefaults(ctx, service.DefaultUserID); err != nil {
		t.Fatalf("init defaults: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "provider-auth", ProjectID: service.DefaultProjectID, Type: "digitalocean", Name: "do"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return svc, appStore, db
}

func updateTestWorkerStatus(ctx context.Context, svc *service.Service, appStore *store.Store, authorization, workerID string, input services.UpdateWorkerStatusBody) (*model.Worker, error) {
	workerCtx, err := authenticateTestWorker(ctx, appStore, authorization, workerID)
	if err != nil {
		return nil, err
	}
	return svc.UpdateWorkerStatus(workerCtx, workerID, input)
}

func authenticateTestWorker(ctx context.Context, appStore *store.Store, authorization, workerID string) (context.Context, error) {
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/workers/"+workerID+"/status", nil)
	req.Header.Set("Authorization", authorization)
	principal, ok, err := (auth.WorkerAuthenticator{Store: appStore}).Authenticate(req)
	if err != nil {
		return ctx, err
	}
	if !ok {
		return ctx, fmt.Errorf("worker authenticator did not match")
	}
	if principal.Type != auth.PrincipalTypeWorker || principal.WorkerID == "" {
		return ctx, fmt.Errorf("unexpected worker principal: %#v", principal)
	}
	return auth.WithPrincipal(ctx, principal), nil
}

func registerTestWorker(ctx context.Context, t *testing.T, svc *service.Service, appStore *store.Store) (string, ed25519.PrivateKey) {
	t.Helper()
	worker := &model.Worker{ID: id.NewString(id.PrefixWorker), ProjectID: service.DefaultProjectID, ProviderInstanceID: "provider-auth"}
	bootstrap := "bootstrap-" + time.Now().String()
	h := sha256.Sum256([]byte(bootstrap))
	if err := appStore.CreateWorkerWithBootstrapToken(ctx, worker, &model.WorkerBootstrapToken{WorkerID: worker.ID, TokenHash: h[:], ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("create worker bootstrap: %v", err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate worker key: %v", err)
	}
	publicKeyText, err := workerauth.EncodePublicKey(publicKey)
	if err != nil {
		t.Fatalf("encode worker key: %v", err)
	}
	_, err = svc.RegisterWorker(ctx, services.RegisterWorkerBody{ProjectId: service.DefaultProjectID, WorkerId: services.OptString{Value: worker.ID, Set: true}, BootstrapToken: bootstrap, PublicKey: publicKeyText})
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}
	return worker.ID, privateKey
}

func signTestWorkerAssertion(t *testing.T, projectID, workerID string, privateKey ed25519.PrivateKey) string {
	t.Helper()
	token, err := workerauth.CreateToken(privateKey, workerauth.Claims{ProjectID: projectID, WorkerID: workerID})
	if err != nil {
		t.Fatalf("create worker assertion: %v", err)
	}
	return token
}

func TestRegisterWorkerSupportsSandboxAssignedWorkerCompatibility(t *testing.T) {
	ctx := context.Background()
	svc, appStore, _ := newWorkerAuthService(t)
	worker := &model.Worker{ID: id.NewString(id.PrefixWorker), ProjectID: service.DefaultProjectID, ProviderInstanceID: "provider-auth"}
	sandboxID := id.NewString(id.PrefixSandbox)
	bootstrap := "bootstrap-" + time.Now().String()
	h := sha256.Sum256([]byte(bootstrap))
	if err := appStore.CreateWorkerWithBootstrapToken(ctx, worker, &model.WorkerBootstrapToken{WorkerID: worker.ID, TokenHash: h[:], ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("create worker bootstrap: %v", err)
	}
	if err := appStore.CreateSandbox(ctx, &model.Sandbox{ID: sandboxID, ProjectID: service.DefaultProjectID, CreatedByUserID: service.DefaultUserID, ProviderInstanceID: &worker.ProviderInstanceID, WorkerID: &worker.ID, Name: "worker sandbox"}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate worker key: %v", err)
	}
	publicKeyText, err := workerauth.EncodePublicKey(publicKey)
	if err != nil {
		t.Fatalf("encode worker key: %v", err)
	}
	resp, err := svc.RegisterWorker(ctx, services.RegisterWorkerBody{ProjectId: service.DefaultProjectID, SandboxId: services.OptString{Value: sandboxID, Set: true}, BootstrapToken: bootstrap, PublicKey: publicKeyText})
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}
	if resp == nil {
		t.Fatal("register response is nil")
	}
}

func TestRegisterWorkerSupportsSyntheticWorkerSandboxIDCompatibility(t *testing.T) {
	ctx := context.Background()
	svc, appStore, _ := newWorkerAuthService(t)
	worker := &model.Worker{ID: id.NewString(id.PrefixWorker), ProjectID: service.DefaultProjectID, ProviderInstanceID: "provider-auth"}
	bootstrap := "bootstrap-" + time.Now().String()
	h := sha256.Sum256([]byte(bootstrap))
	if err := appStore.CreateWorkerWithBootstrapToken(ctx, worker, &model.WorkerBootstrapToken{WorkerID: worker.ID, TokenHash: h[:], ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("create worker bootstrap: %v", err)
	}
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate worker key: %v", err)
	}
	publicKeyText, err := workerauth.EncodePublicKey(publicKey)
	if err != nil {
		t.Fatalf("encode worker key: %v", err)
	}
	resp, err := svc.RegisterWorker(ctx, services.RegisterWorkerBody{ProjectId: service.DefaultProjectID, SandboxId: services.OptString{Value: "worker-" + worker.ID, Set: true}, BootstrapToken: bootstrap, PublicKey: publicKeyText})
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}
	if resp == nil {
		t.Fatal("register response is nil")
	}
}
