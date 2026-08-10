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

	"github.com/obot-platform/discobox/pool-agent/poolauth"
	"github.com/obot-platform/discobox/server/internal/auth"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/events"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/service"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
)

func TestUpdatePoolStatusRequiresValidAgentAssertion(t *testing.T) {
	ctx := context.Background()
	svc, appStore, _, projectID := newPoolAgentTestService(t)
	privateKey := registerTestPool(ctx, t, svc, appStore, projectID, "pool-auth")
	token := signTestAgentAssertion(t, projectID, "pool-auth", privateKey)

	if _, err := updateTestPoolStatus(ctx, svc, appStore, "Bearer "+token, "pool-auth", services.UpdatePoolStatusBody{Ready: true, Schedulable: true, AvailableCpuVcpus: 1}); err != nil {
		t.Fatalf("update pool status with valid assertion: %v", err)
	}
	if _, err := updateTestPoolStatus(ctx, svc, appStore, "Bearer wrong", "pool-auth", services.UpdatePoolStatusBody{Ready: true, Schedulable: true, AvailableCpuVcpus: 1}); err == nil {
		t.Fatal("expected invalid assertion to be rejected")
	}
}

func TestUpdatePoolStatusRejectsCrossPoolAssertion(t *testing.T) {
	ctx := context.Background()
	svc, appStore, _, projectID := newPoolAgentTestService(t)
	registerTestPool(ctx, t, svc, appStore, projectID, "pool-auth")

	otherPool := &model.Pool{ID: "pool-other", ProjectID: projectID, PoolManifest: model.PoolManifest{Name: "other", ProviderInstanceID: "provider-auth"}}
	if err := appStore.CreatePool(ctx, otherPool); err != nil {
		t.Fatalf("create other pool: %v", err)
	}
	otherPrivateKey := registerTestPool(ctx, t, svc, appStore, projectID, "pool-other")
	otherToken := signTestAgentAssertion(t, projectID, "pool-other", otherPrivateKey)

	// A valid assertion for pool-other must not authenticate the pool-auth route.
	if _, err := updateTestPoolStatus(ctx, svc, appStore, "Bearer "+otherToken, "pool-auth", services.UpdatePoolStatusBody{Ready: true, Schedulable: true, AvailableCpuVcpus: 1}); err == nil {
		t.Fatal("expected cross-pool assertion to be rejected")
	}
}

func TestGetPoolIncludesAgentReportedStatus(t *testing.T) {
	ctx := context.Background()
	svc, appStore, db, projectID := newPoolAgentTestService(t)
	privateKey := registerTestPool(ctx, t, svc, appStore, projectID, "pool-auth")
	token := signTestAgentAssertion(t, projectID, "pool-auth", privateKey)
	if _, err := updateTestPoolStatus(ctx, svc, appStore, "Bearer "+token, "pool-auth", services.UpdatePoolStatusBody{
		Ready:                 true,
		Schedulable:           true,
		AvailableCpuVcpus:     2,
		AvailableMemoryBytes:  1024,
		AvailableStorageBytes: 2048,
	}); err != nil {
		t.Fatalf("update pool status: %v", err)
	}
	runtimeState := json.RawMessage(`{"instanceId":"container-1","pool":{"token":"bootstrap-secret","poolId":"pool-auth"}}`)
	if err := db.Write.WithContext(ctx).Model(&model.Pool{}).Where("id = ?", "pool-auth").Update("runtime_state", runtimeState).Error; err != nil {
		t.Fatalf("update runtime state: %v", err)
	}

	pool, err := svc.GetPool(ctx, projectID, "pool-auth")
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if !pool.Ready || !pool.Schedulable || pool.AvailableCPUVCPUs != 2 {
		t.Fatalf("pool status = ready %t schedulable %t cpu %v, want reported values", pool.Ready, pool.Schedulable, pool.AvailableCPUVCPUs)
	}
	// Neither registration nor a heartbeat writes State: it is the reconciler's
	// verdict on the runtime, and no reconcile has run here. The pool reads
	// ready and schedulable — which is what placement gates on — while its
	// state still says what the control plane last converged.
	if pool.State == model.PoolStateActive {
		t.Fatalf("pool state = %q, want the agent path to leave the reconciler's state alone", pool.State)
	}
	response, err := json.Marshal(pool)
	if err != nil {
		t.Fatalf("marshal pool: %v", err)
	}
	if strings.Contains(string(response), "bootstrap-secret") || strings.Contains(string(response), "runtimeState") {
		t.Fatalf("pool response exposes bootstrap secret: %s", response)
	}
}

func newPoolAgentTestService(t *testing.T) (*service.Service, *store.Store, *database.DB, string) {
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
	// Registration marks the pool dirty, so these tests need a real engine.
	svc := service.New(appStore, newTestReconcileEngine(t, db.Write), service.Options{}, broker)
	project, err := svc.InitializeDefaults(ctx, service.DefaultUserID)
	if err != nil {
		t.Fatalf("init defaults: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "provider-auth", ProjectID: project.ID, Type: "digitalocean", Name: "do"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if err := appStore.CreatePool(ctx, &model.Pool{ID: "pool-auth", ProjectID: project.ID, PoolManifest: model.PoolManifest{Name: "pool-auth", ProviderInstanceID: provider.ID}}); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	return svc, appStore, db, project.ID
}

func updateTestPoolStatus(ctx context.Context, svc *service.Service, appStore *store.Store, authorization, poolID string, input services.UpdatePoolStatusBody) (*model.Pool, error) {
	poolCtx, err := authenticateTestPoolAgent(ctx, appStore, authorization, poolID)
	if err != nil {
		return nil, err
	}
	return svc.UpdatePoolStatus(poolCtx, poolID, input)
}

func authenticateTestPoolAgent(ctx context.Context, appStore *store.Store, authorization, poolID string) (context.Context, error) {
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/pools/"+poolID+"/status", nil)
	req.Header.Set("Authorization", authorization)
	principal, ok, err := (auth.PoolAuthenticator{Store: appStore}).Authenticate(req)
	if err != nil {
		return ctx, err
	}
	if !ok {
		return ctx, fmt.Errorf("pool authenticator did not match")
	}
	if principal.Type != auth.PrincipalTypePool || principal.PoolID == "" {
		return ctx, fmt.Errorf("unexpected pool principal: %#v", principal)
	}
	return auth.WithPrincipal(ctx, principal), nil
}

func registerTestPool(ctx context.Context, t *testing.T, svc *service.Service, appStore *store.Store, projectID, poolID string) ed25519.PrivateKey {
	t.Helper()
	bootstrap := "bootstrap-" + poolID
	h := sha256.Sum256([]byte(bootstrap))
	if err := appStore.CreatePoolBootstrapToken(ctx, &model.PoolBootstrapToken{PoolID: poolID, TokenHash: h[:], ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("create pool bootstrap token: %v", err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate agent key: %v", err)
	}
	publicKeyText, err := poolauth.EncodePublicKey(publicKey)
	if err != nil {
		t.Fatalf("encode public key: %v", err)
	}
	if _, err := svc.RegisterPool(ctx, services.RegisterPoolBody{ProjectId: projectID, PoolId: poolID, BootstrapToken: bootstrap, PublicKey: publicKeyText}); err != nil {
		t.Fatalf("register pool: %v", err)
	}
	return privateKey
}

func signTestAgentAssertion(t *testing.T, projectID, poolID string, privateKey ed25519.PrivateKey) string {
	t.Helper()
	token, err := poolauth.CreateTokenWithTTL(privateKey, poolauth.Claims{ProjectID: projectID, PoolID: poolID}, time.Minute)
	if err != nil {
		t.Fatalf("sign agent assertion: %v", err)
	}
	return token
}
