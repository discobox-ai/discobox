package sandboxes

import (
	"context"
	"testing"

	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/reconcile"
	"github.com/obot-platform/discobox/server/internal/store"
)

func TestReportSandboxRemovedRecordsStoppedIntent(t *testing.T) {
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
	worker := &model.Worker{ID: "worker-1", ProjectID: project.ID, ProviderInstanceID: provider.ID}
	if err := appStore.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	sandbox := &model.Sandbox{
		ID: "sandbox-1", ProjectID: project.ID, CreatedByUserID: "user-1", Name: "Sandbox",
		ProviderInstanceID: &provider.ID, WorkerID: &worker.ID,
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState: model.SandboxDesiredStateRunning, Phase: model.SandboxPhaseRunning,
			LastOperationStatus: model.SandboxOperationStatusSuccess, Generation: 3, ObservedGeneration: 3,
		},
	}
	if err := appStore.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	service := NewService(appStore, nil, "user-1", engine)

	if err := service.ReportSandboxRemoved(ctx, worker.ID, sandbox.ID); err != nil {
		t.Fatalf("report sandbox removed: %v", err)
	}
	updated, err := appStore.GetSandbox(ctx, project.ID, sandbox.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if updated.DesiredState != model.SandboxDesiredStateStopped || updated.Phase != model.SandboxPhaseStopping || updated.Generation != 4 {
		t.Fatalf("sandbox desired/phase/generation = %q/%q/%d, want stopped/stopping/4", updated.DesiredState, updated.Phase, updated.Generation)
	}
	if updated.LastOperationStatus != model.SandboxOperationStatusPending {
		t.Fatalf("operation status = %q, want pending", updated.LastOperationStatus)
	}

	if err := service.ReportSandboxRemoved(ctx, worker.ID, sandbox.ID); err != nil {
		t.Fatalf("duplicate report: %v", err)
	}
	duplicate, err := appStore.GetSandbox(ctx, project.ID, sandbox.ID)
	if err != nil {
		t.Fatalf("get sandbox after duplicate: %v", err)
	}
	if duplicate.Generation != 4 {
		t.Fatalf("generation after duplicate = %d, want 4", duplicate.Generation)
	}
}
