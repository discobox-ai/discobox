package sandbox_test

import (
	"context"
	"errors"
	"testing"

	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/server/internal/database"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/store"
)

func TestReconcileSandboxNoCapacityFailsFast(t *testing.T) {
	ctx := context.Background()
	appStore := newReconcilerTestStore(t)

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "digitalocean", Name: "do"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	providerID := provider.ID
	sb := &model.Sandbox{
		ID:                 "sandbox-1",
		ProjectID:          "project-1",
		CreatedByUserID:    "user-1",
		ProviderInstanceID: &providerID,
		Name:               "alpha",
		ResourceLifecycle:  model.NewResourceLifecycle(model.SandboxCreateOperation, nil),
	}
	sb.IncrementGeneration()
	if err := appStore.CreateSandbox(ctx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	reconciler := sandbox.NewSandboxReconciler(appStore, sandbox.WithSandboxProvider(noCapacityProvider{}))
	err := reconciler.ReconcileSandboxJob(ctx, sb.ProjectID, sb.ID, "job-1", sb.Generation)
	if !errors.Is(err, sandbox.ErrNoSandboxCapacity) {
		t.Fatalf("reconcile error = %v, want ErrNoSandboxCapacity", err)
	}

	updated, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if updated.Phase != model.SandboxPhaseFailed || updated.LastOperationStatus != model.SandboxOperationStatusFailed {
		t.Fatalf("sandbox phase/status = %q/%q, want failed/failed", updated.Phase, updated.LastOperationStatus)
	}
	if updated.ErrorMessage == nil || *updated.ErrorMessage != sandbox.ErrNoSandboxCapacity.Error() {
		t.Fatalf("sandbox error message = %v, want no capacity", updated.ErrorMessage)
	}
}

type noCapacityProvider struct {
	sandbox.Provider
}

func (noCapacityProvider) Create(context.Context, sandbox.SandboxRef, []byte, sandbox.CreateOptions) (*sandbox.Sandbox, []byte, error) {
	return nil, nil, sandbox.ErrNoSandboxCapacity
}

func newReconcilerTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	project := &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project", Slug: "project"}
	if err := db.Write.WithContext(ctx).Create(project).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	return store.New(db.Write, db.Read)
}
