package sandboxes

import (
	"context"
	"slices"
	"testing"

	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/reconcile"
	"github.com/obot-platform/discobox/server/internal/store"
)

func TestReportSandboxRemovedStopsOnlyTheCurrentRuntime(t *testing.T) {
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
	project := &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project", Slug: "project"}
	if err := db.Write.WithContext(ctx).Create(project).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Type: "test", Name: "Test"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	pool := &model.Pool{ID: "pool-1", ProjectID: project.ID, Name: "pool-1", ProviderInstanceID: provider.ID}
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	sandbox := &model.Sandbox{
		ID: "sandbox-1", ProjectID: project.ID, CreatedByUserID: "user-1", Name: "Sandbox",
		PoolID: pool.ID,
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState: model.SandboxDesiredStateRunning, Phase: model.SandboxPhaseRunning,
			LastOperationStatus: model.SandboxOperationStatusSuccess, Generation: 3, ObservedGeneration: 3,
		},
	}
	if err := appStore.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	service := NewService(appStore, nil, "user-1", engine)

	// The control plane believes container-a is serving this sandbox.
	sandbox.RuntimeState = []byte(`{"ID":"container-a"}`)
	if err := appStore.UpdateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("record runtime state: %v", err)
	}

	// A report naming a container we have already replaced is stale: it is what an
	// image upgrade's own removal looks like arriving late, and acting on it would
	// stop a sandbox that is running fine on its new container (ADR 0016 §8).
	if err := service.ReportSandboxRemoved(ctx, pool.ID, sandbox.ID, "container-gone"); err != nil {
		t.Fatalf("stale report: %v", err)
	}
	stale, err := appStore.GetSandbox(ctx, project.ID, sandbox.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if stale.DesiredState != model.SandboxDesiredStateRunning || stale.Generation != 3 {
		t.Fatalf("stale report changed state: desired=%q generation=%d", stale.DesiredState, stale.Generation)
	}

	// A report arriving while an operation owns the container is expected, not a
	// loss — the operation is the thing removing it.
	inFlight, err := appStore.GetSandbox(ctx, project.ID, sandbox.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	status := "restarting sandbox"
	inFlight.MarkOperationRunning(&status)
	if err := appStore.UpdateSandbox(ctx, inFlight); err != nil {
		t.Fatalf("mark operation running: %v", err)
	}
	if err := service.ReportSandboxRemoved(ctx, pool.ID, sandbox.ID, "container-a"); err != nil {
		t.Fatalf("in-flight report: %v", err)
	}
	during, err := appStore.GetSandbox(ctx, project.ID, sandbox.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if during.DesiredState != model.SandboxDesiredStateRunning {
		t.Fatalf("in-flight report changed intent to %q", during.DesiredState)
	}

	// At steady state, losing the container we believe in is a genuine loss, and
	// the sandbox should stop trying rather than be silently resurrected.
	settled, err := appStore.GetSandbox(ctx, project.ID, sandbox.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	settled.CompleteOperation(model.SandboxPhaseRunning, nil)
	if err := appStore.UpdateSandbox(ctx, settled); err != nil {
		t.Fatalf("complete operation: %v", err)
	}
	before := settled.Generation
	if err := service.ReportSandboxRemoved(ctx, pool.ID, sandbox.ID, "container-a"); err != nil {
		t.Fatalf("genuine report: %v", err)
	}
	stopped, err := appStore.GetSandbox(ctx, project.ID, sandbox.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if stopped.DesiredState != model.SandboxDesiredStateStopped || stopped.Generation != before+1 {
		t.Fatalf("desired/generation = %q/%d, want stopped/%d", stopped.DesiredState, stopped.Generation, before+1)
	}
	if !slices.ContainsFunc(mustListDirty(ctx, t, engine), func(d reconcile.DirtyResource) bool {
		return d.ResourceID == SandboxDirtyID(project.ID, sandbox.ID)
	}) {
		t.Fatal("want the sandbox marked for reconcile")
	}

	// Already stopped: nothing more to say.
	if err := service.ReportSandboxRemoved(ctx, pool.ID, sandbox.ID, "container-a"); err != nil {
		t.Fatalf("duplicate report: %v", err)
	}
	duplicate, err := appStore.GetSandbox(ctx, project.ID, sandbox.ID)
	if err != nil {
		t.Fatalf("get sandbox after duplicate: %v", err)
	}
	if duplicate.Generation != stopped.Generation {
		t.Fatalf("generation after duplicate = %d, want %d", duplicate.Generation, stopped.Generation)
	}
}

func mustListDirty(ctx context.Context, t *testing.T, engine *reconcile.Engine) []reconcile.DirtyResource {
	t.Helper()
	dirty, err := engine.ListDirty(ctx, SandboxResourceType)
	if err != nil {
		t.Fatalf("list dirty: %v", err)
	}
	return dirty
}
