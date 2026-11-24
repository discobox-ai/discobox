package database_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/discobox-ai/x/gormdb"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/resources/sandboxes"
)

// A discobox still holding a row in a variable it runs on, delivered under a
// binding stored before one was refused (#1545), has its sentinel delivery
// marked at upgrade, so the row is taken away without a restart. One holding
// none, and one archived, is not marked.
func TestMigrateMarksTheHoldersOfARuntimeVariable(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{Driver: gormdb.DriverSQLite, DSN: "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}
	if _, err := reconcile.New(db.Write, reconcile.Options{}); err != nil {
		t.Fatalf("reconcile table: %v", err)
	}
	for _, row := range []any{
		&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"},
		&model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "docker"},
		&model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: "provider-1"}},
	} {
		if err := db.Write.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, sandbox := range []struct{ id, desired string }{
		{"sbx-path", model.DesiredStatePresent}, {"sbx-token", model.DesiredStatePresent}, {"sbx-archived", model.DesiredStateArchived},
	} {
		if err := db.Write.Exec(`INSERT INTO sandboxes (id, project_id, pool_id, created_by_user_id, name, desired_state) VALUES (?, 'project-1', 'pool-1', 'user-1', ?, ?)`,
			sandbox.id, sandbox.id, sandbox.desired).Error; err != nil {
			t.Fatalf("sandbox %s: %v", sandbox.id, err)
		}
	}
	for _, row := range []model.SandboxSecret{
		{ID: "ss-1", ProjectID: "project-1", SandboxID: "sbx-path", SecretID: "sec-1", EnvName: "PATH", Sentinel: "s1"},
		{ID: "ss-2", ProjectID: "project-1", SandboxID: "sbx-path", SecretID: "sec-1", EnvName: "GH_TOKEN", Sentinel: "s2"},
		{ID: "ss-3", ProjectID: "project-1", SandboxID: "sbx-token", SecretID: "sec-1", EnvName: "GH_TOKEN", Sentinel: "s3"},
		{ID: "ss-4", ProjectID: "project-1", SandboxID: "sbx-archived", SecretID: "sec-1", EnvName: "LD_PRELOAD", Sentinel: "s4"},
	} {
		if err := db.Write.Create(&row).Error; err != nil {
			t.Fatalf("row %s: %v", row.ID, err)
		}
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var marks []string
	if err := db.Write.Table("reconcile_dirty").Order("resource_type, resource_id").
		Select("resource_type || ' ' || resource_id").Scan(&marks).Error; err != nil {
		t.Fatal(err)
	}
	if want := []string{sandboxes.SentinelsResourceType + " " + sandboxes.SandboxDirtyID("project-1", "sbx-path")}; !reflect.DeepEqual(marks, want) {
		t.Fatalf("dirty = %v, want the present holder of PATH alone", marks)
	}
}
