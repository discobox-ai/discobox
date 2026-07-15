package database_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/obot-platform/discobox/gormdb"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
)

func TestNewCreatesSQLiteDatabase(t *testing.T) {
	dir := t.TempDir()
	dsn := "sqlite3://" + filepath.Join(dir, "discobox.db")
	db, err := database.New(database.Config{
		Driver: gormdb.DriverSQLite,
		DSN:    dsn,
	})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close database: %v", err)
		}
	})

	if db.Write == nil {
		t.Fatalf("database returned nil write handle")
	}
	if !fileExists(filepath.Join(dir, "discobox.db")) {
		t.Fatalf("expected application database to exist")
	}
}

func TestMigrateMigratesSingleSchema(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{
		Driver: gormdb.DriverSQLite,
		DSN:    "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db"),
	})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close database: %v", err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate database: %v", err)
	}

	for _, table := range []string{"users", "projects", "sandboxes", "sandbox_provider_instances", "workers", "project_events"} {
		if !db.Write.Migrator().HasTable(table) {
			t.Fatalf("schema missing table %s", table)
		}
	}
	if db.Write.Migrator().HasTable("organizations") {
		t.Fatalf("schema unexpectedly has organizations table")
	}
}

// TestMigrateDropsJobQueueArtifactsWithForeignKeys reproduces upgrading a
// database from the job-queue era: workers/sandboxes still carry last_job_id
// and jobqueue tables exist, with live FK-linked rows. SQLite drops columns by
// rebuilding the table, which fails under foreign_keys=ON unless the migration
// disables the pragma around the rebuild.
func TestMigrateDropsJobQueueArtifactsWithForeignKeys(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{
		Driver: gormdb.DriverSQLite,
		DSN:    "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db"),
	})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close database: %v", err)
		}
	})

	// Current schema first, then re-add the legacy job-queue artifacts.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}
	for _, stmt := range []string{
		"ALTER TABLE workers ADD COLUMN `last_job_id` text",
		"ALTER TABLE sandboxes ADD COLUMN `last_job_id` text",
		"CREATE TABLE jobqueue_jobs (id text PRIMARY KEY)",
		"CREATE TABLE jobqueue_leaders (id text PRIMARY KEY)",
	} {
		if err := db.Write.Exec(stmt).Error; err != nil {
			t.Fatalf("recreate legacy artifact %q: %v", stmt, err)
		}
	}

	// FK-linked rows spanning the tables that get rebuilt.
	project := &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project", Slug: "project"}
	if err := db.Write.Create(project).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Type: "docker", Name: "Docker"}
	if err := db.Write.Create(provider).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	worker := &model.Worker{ID: "worker-1", ProjectID: project.ID, ProviderInstanceID: provider.ID, Identity: "worker-1"}
	if err := db.Write.Create(worker).Error; err != nil {
		t.Fatalf("create worker: %v", err)
	}
	workerID := worker.ID
	sandbox := &model.Sandbox{ID: "sandbox-1", ProjectID: project.ID, CreatedByUserID: "user-1", Name: "sandbox", WorkerID: &workerID}
	if err := db.Write.Create(sandbox).Error; err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("upgrade migrate: %v", err)
	}

	for _, table := range []string{"jobqueue_jobs", "jobqueue_leaders"} {
		if db.Write.Migrator().HasTable(table) {
			t.Fatalf("%s still exists after migration", table)
		}
	}
	for _, m := range []any{&model.Sandbox{}, &model.Worker{}} {
		if db.Write.Migrator().HasColumn(m, "last_job_id") {
			t.Fatalf("%T still has last_job_id after migration", m)
		}
	}
	// The rebuilt tables must retain their rows and FK links.
	var got model.Sandbox
	if err := db.Write.First(&got, "id = ?", sandbox.ID).Error; err != nil {
		t.Fatalf("sandbox lost in rebuild: %v", err)
	}
	if got.WorkerID == nil || *got.WorkerID != worker.ID {
		t.Fatalf("sandbox worker link = %v, want %s", got.WorkerID, worker.ID)
	}
	var keptWorker model.Worker
	if err := db.Write.First(&keptWorker, "id = ?", worker.ID).Error; err != nil {
		t.Fatalf("worker lost in rebuild: %v", err)
	}
	// FK enforcement must remain on for normal connections afterwards.
	var fk int
	if err := db.Write.Raw("PRAGMA foreign_keys").Scan(&fk).Error; err != nil {
		t.Fatalf("read foreign_keys pragma: %v", err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys pragma = %d, want 1", fk)
	}
}

func fileExists(path string) bool {
	matches, err := filepath.Glob(path)
	return err == nil && len(matches) == 1
}
