package database_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/resources/sandboxes"
	"github.com/discobox-ai/x/gormdb"
)

// The first start since the value generation changed shape (it names its
// secret since ADR 26-11-19-927 §5) marks every discobox holding a sentinel
// for delivery, so its pool is sent the new generations and its proxy caches
// again; the record of it is written with the marks, and a later start marks
// nothing.
func TestMigrateRedeliversSentinelsOnceWhenTheGenerationChangesShape(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{Driver: gormdb.DriverSQLite, DSN: "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	engine, err := reconcile.New(db.Write, reconcile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	write := db.Write.WithContext(ctx)
	if err := write.Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := write.Create(&model.SandboxSecret{ID: "ss-1", ProjectID: "project-1", SandboxID: "sbx-1", SecretID: "sec-1", EnvName: "GH_TOKEN", Sentinel: "SENTINEL-1"}).Error; err != nil {
		t.Fatal(err)
	}
	// The database as the release before had it: never marked.
	if err := write.Exec(`DELETE FROM server_state WHERE key = 'sentinel-generations-name-secret'`).Error; err != nil {
		t.Fatal(err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dirty, err := engine.ListDirty(ctx, sandboxes.SentinelsResourceType)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 1 || dirty[0].ResourceID != sandboxes.SandboxDirtyID("project-1", "sbx-1") {
		t.Fatalf("dirty = %+v, want sbx-1's sentinels marked once", dirty)
	}
	if err := write.Exec(`DELETE FROM reconcile_dirty`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if dirty, _ := engine.ListDirty(ctx, sandboxes.SentinelsResourceType); len(dirty) != 0 {
		t.Fatalf("dirty = %+v after a second start, want nothing", dirty)
	}
}
