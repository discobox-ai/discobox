package database_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/x/gormdb"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// The use index from before additions is replaced by one over plain use
// bindings alone (ADR 26-11-03-329 §1): a database that has it loses it, and
// every binding it held is held as before, while additions in one variable
// are no collision.
func TestMigrateNarrowsTheUseIndexToPlainBindings(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{Driver: gormdb.DriverSQLite, DSN: "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}
	// The database as it was: the old index over every use binding.
	if err := db.Write.Exec("DROP INDEX idx_secret_binding_plain_use").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Write.Exec("CREATE UNIQUE INDEX idx_secret_binding_use ON secret_bindings (target_kind, target_id, env_name) WHERE use").Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	migrator := db.Write.Migrator()
	if migrator.HasIndex(&model.SecretBinding{}, "idx_secret_binding_use") {
		t.Fatal("the old use index survived the upgrade")
	}
	if !migrator.HasIndex(&model.SecretBinding{}, "idx_secret_binding_plain_use") {
		t.Fatal("no plain use index after the upgrade")
	}
	if err := db.Write.Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatal(err)
	}
	binding := func(id string, adds bool) *model.SecretBinding {
		return &model.SecretBinding{ID: id, ProjectID: "project-1", SecretID: "sec-1", TargetKind: model.SecretBindingTargetSandbox, TargetID: "sb-1", Use: true, Adds: adds, EnvName: "GH_TOKEN"}
	}
	for _, row := range []*model.SecretBinding{binding("bind-plain", false), binding("bind-add-1", true), binding("bind-add-2", true)} {
		if err := db.Write.Create(row).Error; err != nil {
			t.Fatalf("create %s: %v", row.ID, err)
		}
	}
	if err := db.Write.Create(binding("bind-plain-2", false)).Error; err == nil {
		t.Fatal("a second plain use binding in one variable was stored")
	}
}
