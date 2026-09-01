package sandboxes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// progressFixture is a ready sandbox on a pool, for the report paths that write
// to it.
func progressFixture(t *testing.T) (context.Context, *Service, *store.Store) {
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
	engine, err := reconcile.New(db.Write, reconcile.Options{SingleNode: true})
	if err != nil {
		t.Fatalf("create reconcile engine: %v", err)
	}
	project := &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}
	if err := db.Write.WithContext(ctx).Create(project).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Type: "test", Name: "Test"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	pool := &model.Pool{ID: "pool-1", ProjectID: project.ID, PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: provider.ID}}
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	sandbox := &model.Sandbox{
		ID: "sandbox-1", ProjectID: project.ID, CreatedByUserID: "user-1", Name: "sandbox-1", PoolID: pool.ID,
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState: model.DesiredStatePresent,
			State:        model.SandboxStateReady,
		},
		RuntimeState: model.SandboxRuntimeStateStopped,
	}
	if err := appStore.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	return ctx, NewService(appStore, nil, "user-1", engine), appStore
}

// Progress is recorded every time, because a client reads it to say what it is
// waiting for. It must not disturb the sandbox's state.
func TestProgressReportIsRecorded(t *testing.T) {
	ctx, service, appStore := progressFixture(t)

	payload := json.RawMessage(`{"pull":{"image":"ghcr.io/example/sandbox:latest","current":50,"total":100,"layers":3,"layersComplete":1}}`)
	at := time.Now().UTC()
	err := service.ReportSandboxProgress(ctx, "pool-1", at, []store.SandboxProgressReport{
		{SandboxID: "sandbox-1", Progress: payload},
	})
	if err != nil {
		t.Fatalf("ReportSandboxProgress: %v", err)
	}

	sandbox, err := appStore.GetSandbox(ctx, "project-1", "sandbox-1")
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if !strings.Contains(string(sandbox.ProvisionProgress), "ghcr.io/example/sandbox:latest") {
		t.Fatalf("progress = %s, want the reported pull", sandbox.ProvisionProgress)
	}
	if sandbox.ProvisionProgressAt == nil {
		t.Fatal("progress timestamp was not recorded")
	}
	// Progress says nothing about power state, and must not be mistaken for it.
	if sandbox.RuntimeState != model.SandboxRuntimeStateStopped {
		t.Fatalf("runtime state = %q, want it untouched at %q", sandbox.RuntimeState, model.SandboxRuntimeStateStopped)
	}
}

// A pool reporting progress for a sandbox it does not host, or one this control
// plane has never heard of, is not an error worth failing the whole batch over —
// but it must not write anything either.
func TestProgressReportIgnoresSandboxesThePoolDoesNotHost(t *testing.T) {
	ctx, service, appStore := progressFixture(t)

	err := service.ReportSandboxProgress(ctx, "pool-1", time.Now().UTC(), []store.SandboxProgressReport{
		{SandboxID: "sandbox-does-not-exist", Progress: json.RawMessage(`{"pull":{"image":"img"}}`)},
	})
	if err != nil {
		t.Fatalf("ReportSandboxProgress: %v", err)
	}
	sandbox, err := appStore.GetSandbox(ctx, "project-1", "sandbox-1")
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if len(sandbox.ProvisionProgress) != 0 {
		t.Fatalf("unrelated sandbox got progress: %s", sandbox.ProvisionProgress)
	}
}
