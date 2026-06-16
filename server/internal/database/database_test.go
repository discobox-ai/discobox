package database_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/obot-platform/discobox/gormdb"
	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/store"
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

	for _, table := range []string{"users", "projects", "sandboxes", "sandbox_provider_instances", "workers", "project_events", "jobqueue_jobs"} {
		if !db.Write.Migrator().HasTable(table) {
			t.Fatalf("schema missing table %s", table)
		}
	}
	if db.Write.Migrator().HasTable("organizations") {
		t.Fatalf("schema unexpectedly has organizations table")
	}
}

func TestMigrateDropsLegacyTenantIDColumns(t *testing.T) {
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

	if err := db.Write.Exec(`
		CREATE TABLE users (
			id text PRIMARY KEY,
			tenant_id text NOT NULL,
			email text NOT NULL,
			name text,
			avatar_url text,
			provider text NOT NULL,
			subject text NOT NULL,
			created_at datetime,
			updated_at datetime,
			deleted_at datetime
		)
	`).Error; err != nil {
		t.Fatalf("create legacy users table: %v", err)
	}
	if err := db.Write.Exec(`CREATE INDEX idx_legacy_users_tenant_email ON users (tenant_id, email)`).Error; err != nil {
		t.Fatalf("create legacy tenant index: %v", err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	if db.Write.Migrator().HasColumn("users", "tenant_id") {
		t.Fatalf("legacy tenant_id column was not dropped")
	}
	if err := db.Write.WithContext(ctx).Create(&model.User{
		ID:       "default-user",
		Email:    "default@example.com",
		Provider: "default",
		Subject:  "default-user",
	}).Error; err != nil {
		t.Fatalf("insert tenantless user after migration: %v", err)
	}
}

func TestMigrateDropsLegacyTenantIDFromJobQueue(t *testing.T) {
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

	if err := db.Write.Exec(`
		CREATE TABLE jobqueue_jobs (
			id text PRIMARY KEY,
			tenant_id text NOT NULL,
			type text NOT NULL,
			payload text NOT NULL,
			status text NOT NULL,
			priority integer NOT NULL DEFAULT 0,
			attempts integer NOT NULL DEFAULT 0,
			max_attempts integer NOT NULL DEFAULT 1,
			error text,
			worker_id text,
			scheduled_at datetime NOT NULL,
			started_at datetime,
			completed_at datetime,
			resource_type text,
			resource_id text,
			active_resource_key text,
			created_at datetime,
			updated_at datetime
		)
	`).Error; err != nil {
		t.Fatalf("create legacy jobqueue_jobs table: %v", err)
	}
	if err := db.Write.Exec(`CREATE INDEX idx_legacy_jobs_tenant_status ON jobqueue_jobs (tenant_id, status)`).Error; err != nil {
		t.Fatalf("create legacy job tenant index: %v", err)
	}
	if err := db.Write.Exec(`
		CREATE TABLE jobqueue_leaders (
			id text PRIMARY KEY,
			tenant_id text NOT NULL,
			worker_id text NOT NULL,
			heartbeat_at datetime NOT NULL,
			acquired_at datetime NOT NULL
		)
	`).Error; err != nil {
		t.Fatalf("create legacy jobqueue_leaders table: %v", err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	if db.Write.Migrator().HasColumn("jobqueue_jobs", "tenant_id") {
		t.Fatalf("legacy jobqueue_jobs tenant_id column was not dropped")
	}
	if db.Write.Migrator().HasColumn("jobqueue_leaders", "tenant_id") {
		t.Fatalf("legacy jobqueue_leaders tenant_id column was not dropped")
	}

	appStore := store.New(db.Write, db.Read)
	job := &orchestration.Job{
		Type:        orchestration.Type("test"),
		Payload:     []byte(`{}`),
		Status:      orchestration.StatusPending,
		MaxAttempts: 1,
		ScheduledAt: time.Now().UTC(),
		Resource:    orchestration.Resource{Type: "test", ID: "job"},
	}
	if err := appStore.CreateJob(ctx, job); err != nil {
		t.Fatalf("insert tenantless job after migration: %v", err)
	}
}

func TestMigrateRebuildsLegacyServerStateTenantPrimaryKey(t *testing.T) {
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

	if err := db.Write.Exec(`
		CREATE TABLE server_state (
			tenant_id text NOT NULL,
			key text NOT NULL,
			value text,
			created_at datetime,
			updated_at datetime,
			PRIMARY KEY (tenant_id, key)
		)
	`).Error; err != nil {
		t.Fatalf("create legacy server_state table: %v", err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	if db.Write.Migrator().HasColumn("server_state", "tenant_id") {
		t.Fatalf("legacy server_state tenant_id column was not dropped")
	}
	if err := db.Write.WithContext(ctx).Create(&model.ServerState{
		Key:   "initialized",
		Value: []byte(`true`),
	}).Error; err != nil {
		t.Fatalf("insert tenantless server state after migration: %v", err)
	}
}

func fileExists(path string) bool {
	matches, err := filepath.Glob(path)
	return err == nil && len(matches) == 1
}
