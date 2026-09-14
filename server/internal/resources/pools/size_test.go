package pools

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// The provider types here are invented on purpose. What a pool may set is
// whatever its provider declares, and a test that used a real provider type
// could pass just as well against a list of type names in the pool service.
const (
	sizedType   = "test-sized-vm"    // declares CPU and memory
	unsizedType = "test-shared-host" // declares nothing
)

func newSizeFixture(t *testing.T) *Service {
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
	appStore := store.New(db.Write, db.Read)
	if err := appStore.UpsertProject(ctx, &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "project-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	for _, provider := range []model.SandboxProviderInstance{
		{ID: "prov-sized", ProjectID: "project-1", Type: sizedType, Name: "Sized"},
		{ID: "prov-unsized", ProjectID: "project-1", Type: unsizedType, Name: "Shared"},
	} {
		if err := appStore.CreateSandboxProviderInstance(ctx, &provider); err != nil {
			t.Fatalf("create provider %s: %v", provider.ID, err)
		}
	}

	manager := sandbox.NewProviderManager()
	manager.RegisterProviderDefinition(sizedType, sandbox.ProviderDefinition{
		Name:           sizedType,
		PoolSizeFields: []sandbox.PoolSizeField{sandbox.PoolSizeCPU, sandbox.PoolSizeMemory},
	})
	manager.RegisterProviderDefinition(unsizedType, sandbox.ProviderDefinition{Name: unsizedType})
	return NewService(appStore, manager, nil)
}

func requireBadRequest(t *testing.T, err error, mentions ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("request was accepted, want it refused")
	}
	var status apperrors.StatusError
	if !errors.As(err, &status) || status.Status != http.StatusBadRequest {
		t.Fatalf("error = %v, want a 400", err)
	}
	for _, want := range mentions {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}

// A setting the provider declares is accepted; one it does not declare is
// refused rather than stored and ignored, and the refusal names the field and
// what the provider does act on.
func TestCreatePoolHoldsSizesToTheProvidersDeclaration(t *testing.T) {
	svc := newSizeFixture(t)
	ctx := context.Background()

	pool, err := svc.CreatePool(ctx, "project-1", services.CreatePoolBody{
		Name: "sized", ProviderInstanceId: "prov-sized",
		CpuVcpus: serverapi.NewOptFloat64(4), MemoryBytes: serverapi.NewOptInt64(8 << 30),
	})
	if err != nil {
		t.Fatalf("CreatePool with declared fields: %v", err)
	}
	if pool.CPUVCPUs != 4 || pool.MemoryBytes != 8<<30 {
		t.Fatalf("pool size = %v vCPUs, %d bytes; want what was set", pool.CPUVCPUs, pool.MemoryBytes)
	}

	_, err = svc.CreatePool(ctx, "project-1", services.CreatePoolBody{
		Name: "sized-storage", ProviderInstanceId: "prov-sized",
		StorageBytes: serverapi.NewOptInt64(100 << 30),
	})
	requireBadRequest(t, err, "storageBytes", "acts only on cpuVcpus, memoryBytes")

	_, err = svc.CreatePool(ctx, "project-1", services.CreatePoolBody{
		Name: "shared", ProviderInstanceId: "prov-unsized",
		MemoryBytes: serverapi.NewOptInt64(8 << 30),
	})
	requireBadRequest(t, err, "memoryBytes", unsizedType, "takes no pool size settings")

	// Zero is unset, and every provider accepts unset; the copy path and older
	// clients send explicit zeros.
	if _, err := svc.CreatePool(ctx, "project-1", services.CreatePoolBody{
		Name: "shared-zero", ProviderInstanceId: "prov-unsized",
		CpuVcpus: serverapi.NewOptFloat64(0), MemoryBytes: serverapi.NewOptInt64(0), StorageBytes: serverapi.NewOptInt64(0),
	}); err != nil {
		t.Fatalf("CreatePool with explicit zeros: %v", err)
	}

	_, err = svc.CreatePool(ctx, "project-1", services.CreatePoolBody{
		Name: "negative", ProviderInstanceId: "prov-sized", CpuVcpus: serverapi.NewOptFloat64(-1),
	})
	requireBadRequest(t, err, "cpuVcpus", "negative")
}

// An update is judged on the fields it names. Clearing a value is always
// allowed, and a rename never fails over a size stored before providers
// declared what they act on.
func TestUpdatePoolJudgesOnlyTheFieldsItSets(t *testing.T) {
	svc := newSizeFixture(t)
	ctx := context.Background()

	// A pool from before the declaration existed: stored directly, holding a
	// size its provider never acted on.
	stale := &model.Pool{ID: "pool-stale", ProjectID: "project-1", PoolManifest: model.PoolManifest{
		Name: "stale", ProviderInstanceID: "prov-unsized", MemoryBytes: 8 << 30,
	}}
	if err := svc.store.CreatePool(ctx, stale); err != nil {
		t.Fatalf("store stale pool: %v", err)
	}

	if _, err := svc.UpdatePool(ctx, "project-1", "pool-stale", services.UpdatePoolBody{Name: serverapi.NewOptString("renamed")}); err != nil {
		t.Fatalf("rename of a pool with a stale size: %v", err)
	}

	_, err := svc.UpdatePool(ctx, "project-1", "pool-stale", services.UpdatePoolBody{CpuVcpus: serverapi.NewOptFloat64(2)})
	requireBadRequest(t, err, "cpuVcpus", unsizedType)

	cleared, err := svc.UpdatePool(ctx, "project-1", "pool-stale", services.UpdatePoolBody{MemoryBytes: serverapi.NewOptInt64(0)})
	if err != nil {
		t.Fatalf("clearing a size the provider never acted on: %v", err)
	}
	if cleared.MemoryBytes != 0 {
		t.Fatalf("memory = %d after clearing, want 0", cleared.MemoryBytes)
	}
}
