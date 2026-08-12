package store_test

import (
	"context"
	"testing"

	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/store"
)

func newSandboxNameTestStore(t *testing.T) (*store.Store, *database.DB) {
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
	for _, project := range []*model.Project{
		{ID: "project-1", OwnerUserID: "user-1", Name: "Project"},
		{ID: "project-2", OwnerUserID: "user-1", Name: "Other"},
	} {
		if err := db.Write.WithContext(ctx).Create(project).Error; err != nil {
			t.Fatalf("create project: %v", err)
		}
	}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "Docker"}
	if err := db.Write.WithContext(ctx).Create(provider).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: provider.ID}}
	if err := db.Write.WithContext(ctx).Create(pool).Error; err != nil {
		t.Fatalf("create pool: %v", err)
	}
	return store.New(db.Write, db.Read), db
}

func newNamedSandbox(id, projectID, name string) *model.Sandbox {
	return &model.Sandbox{ID: id, ProjectID: projectID, PoolID: "pool-1", CreatedByUserID: "user-1", Name: name}
}

// TestSandboxNamesAreUniqueWithinAProject is the constraint that lets a name be
// used as an addressable handle: an ssh_config Host alias has to mean exactly
// one sandbox.
func TestSandboxNamesAreUniqueWithinAProject(t *testing.T) {
	_, db := newSandboxNameTestStore(t)
	ctx := context.Background()

	if err := db.Write.WithContext(ctx).Create(newNamedSandbox("sbx_first0", "project-1", "twin")).Error; err != nil {
		t.Fatalf("create first sandbox: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(newNamedSandbox("sbx_second", "project-1", "twin")).Error; err == nil {
		t.Fatal("a duplicate name in the same project was accepted")
	}
	// Scoped to the project, like pools and harness configs.
	if err := db.Write.WithContext(ctx).Create(newNamedSandbox("sbx_other0", "project-2", "twin")).Error; err != nil {
		t.Fatalf("the same name in another project was rejected: %v", err)
	}
}

// TestSandboxNamesFreeUpOnDelete: deletes are hard deletes (ADR 0010), so a
// name is reusable once its sandbox is gone rather than being retired forever.
func TestSandboxNamesFreeUpOnDelete(t *testing.T) {
	_, db := newSandboxNameTestStore(t)
	ctx := context.Background()

	if err := db.Write.WithContext(ctx).Create(newNamedSandbox("sbx_first0", "project-1", "reused")).Error; err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if err := db.Write.WithContext(ctx).Delete(&model.Sandbox{}, "id = ?", "sbx_first0").Error; err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(newNamedSandbox("sbx_second", "project-1", "reused")).Error; err != nil {
		t.Fatalf("name was not released by the delete: %v", err)
	}
}

func TestSandboxNameTaken(t *testing.T) {
	appStore, db := newSandboxNameTestStore(t)
	ctx := context.Background()
	if err := db.Write.WithContext(ctx).Create(newNamedSandbox("sbx_first0", "project-1", "taken")).Error; err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	for _, tc := range []struct {
		name      string
		projectID string
		sandbox   string
		want      bool
	}{
		{name: "taken in this project", projectID: "project-1", sandbox: "taken", want: true},
		{name: "free in this project", projectID: "project-1", sandbox: "free", want: false},
		{name: "taken only in another project", projectID: "project-2", sandbox: "taken", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := appStore.SandboxNameTaken(ctx, tc.projectID, tc.sandbox)
			if err != nil {
				t.Fatalf("SandboxNameTaken: %v", err)
			}
			if got != tc.want {
				t.Fatalf("SandboxNameTaken(%q, %q) = %v, want %v", tc.projectID, tc.sandbox, got, tc.want)
			}
		})
	}
}
