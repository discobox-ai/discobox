package database_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/x/gormdb"
)

// Retiring the API/model must preserve old history and allow new sandbox writes.
func TestMigratePreservesRetiredApplyHistory(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{Driver: gormdb.DriverSQLite, DSN: "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if db.Write.Migrator().HasColumn("sandboxes", "applied_commits") {
		t.Fatal("new schema includes retired apply history")
	}
	if err := db.Write.Exec("ALTER TABLE sandboxes ADD COLUMN applied_commits text").Error; err != nil {
		t.Fatal(err)
	}
	project := &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Type: "docker", Name: "Docker"}
	pool := &model.Pool{ID: "pool-1", ProjectID: project.ID, PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: provider.ID}}
	sandbox := &model.Sandbox{ID: "sandbox-1", ProjectID: project.ID, PoolID: pool.ID, CreatedByUserID: "user-1", Name: "sandbox"}
	for _, row := range []any{project, provider, pool, sandbox} {
		if err := db.Write.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	history := `[{"slug":"primary","commit":"1111111111111111111111111111111111111111","hostCommit":"2222222222222222222222222222222222222222","hostId":"host-1","hostPath":"/src/project","appliedAt":"2026-10-01T00:00:00Z"}]`
	if err := db.Write.Table("sandboxes").Where("id = ?", sandbox.ID).Update("applied_commits", history).Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("upgrade: %v", err)
		}
	}
	sandbox.Name = "renamed"
	if err := db.Write.Save(sandbox).Error; err != nil {
		t.Fatalf("update existing sandbox: %v", err)
	}
	var stored struct {
		Name           string
		AppliedCommits string
	}
	if err := db.Write.Table("sandboxes").Select("name", "applied_commits").Where("id = ?", sandbox.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Name != sandbox.Name || stored.AppliedCommits != history {
		t.Fatalf("stored sandbox = %+v, want renamed sandbox with unchanged history", stored)
	}
	fresh := &model.Sandbox{ID: "sandbox-2", ProjectID: project.ID, PoolID: pool.ID, CreatedByUserID: "user-1", Name: "fresh"}
	if err := db.Write.Create(fresh).Error; err != nil {
		t.Fatalf("create sandbox after upgrade: %v", err)
	}
	var noHistory bool
	if err := db.Write.Raw("SELECT applied_commits IS NULL FROM sandboxes WHERE id = ?", fresh.ID).Scan(&noHistory).Error; err != nil {
		t.Fatal(err)
	}
	if !noHistory {
		t.Fatal("new sandbox populated retired apply history")
	}
}
