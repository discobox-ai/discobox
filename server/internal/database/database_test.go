package database_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

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

	for _, table := range []string{"users", "projects", "sandboxes", "sandbox_provider_instances", "pools", "project_events"} {
		if !db.Write.Migrator().HasTable(table) {
			t.Fatalf("schema missing table %s", table)
		}
	}
	if db.Write.Migrator().HasTable("organizations") {
		t.Fatalf("schema unexpectedly has organizations table")
	}
}

func TestMigrateRenamesLocalVMProvidersToLibkrun(t *testing.T) {
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
		t.Fatalf("initial migrate: %v", err)
	}

	project := &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}
	if err := db.Write.Create(project).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	provider := &model.SandboxProviderInstance{
		ID:        "provider-1",
		ProjectID: project.ID,
		Type:      "local-vm",
		Name:      "Existing libkrun provider",
	}
	if err := db.Write.Create(provider).Error; err != nil {
		t.Fatalf("create legacy provider: %v", err)
	}
	pool := &model.Pool{ID: "pool-1", ProjectID: project.ID, PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: provider.ID}}
	if err := db.Write.Create(pool).Error; err != nil {
		t.Fatalf("create pool: %v", err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("upgrade migrate: %v", err)
	}

	var got model.SandboxProviderInstance
	if err := db.Write.First(&got, "id = ?", provider.ID).Error; err != nil {
		t.Fatalf("read migrated provider: %v", err)
	}
	if got.Type != "libkrun" {
		t.Fatalf("provider type = %q, want libkrun", got.Type)
	}
	var gotPool model.Pool
	if err := db.Write.First(&gotPool, "id = ?", pool.ID).Error; err != nil {
		t.Fatalf("read preserved pool: %v", err)
	}
	if gotPool.ProviderInstanceID != provider.ID {
		t.Fatalf("pool provider = %q, want %s", gotPool.ProviderInstanceID, provider.ID)
	}
}

// TestMigrateDropsJobQueueArtifactsWithForeignKeys reproduces upgrading a
// database from the job-queue era: sandboxes still carry last_job_id and
// jobqueue tables exist, with live FK-linked rows. SQLite drops columns by
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
		"ALTER TABLE sandboxes ADD COLUMN `last_job_id` text",
		"CREATE TABLE jobqueue_jobs (id text PRIMARY KEY)",
		"CREATE TABLE jobqueue_leaders (id text PRIMARY KEY)",
	} {
		if err := db.Write.Exec(stmt).Error; err != nil {
			t.Fatalf("recreate legacy artifact %q: %v", stmt, err)
		}
	}

	// FK-linked rows spanning the tables that get rebuilt.
	project := &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}
	if err := db.Write.Create(project).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Type: "docker", Name: "Docker"}
	if err := db.Write.Create(provider).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	pool := &model.Pool{ID: "pool-1", ProjectID: project.ID, PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: provider.ID}}
	if err := db.Write.Create(pool).Error; err != nil {
		t.Fatalf("create pool: %v", err)
	}
	sandbox := &model.Sandbox{ID: "sandbox-1", ProjectID: project.ID, PoolID: pool.ID, CreatedByUserID: "user-1", Name: "sandbox"}
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
	for _, m := range []any{&model.Sandbox{}} {
		if db.Write.Migrator().HasColumn(m, "last_job_id") {
			t.Fatalf("%T still has last_job_id after migration", m)
		}
	}
	// The rebuilt tables must retain their rows and FK links.
	var got model.Sandbox
	if err := db.Write.First(&got, "id = ?", sandbox.ID).Error; err != nil {
		t.Fatalf("sandbox lost in rebuild: %v", err)
	}
	if got.PoolID != pool.ID {
		t.Fatalf("sandbox pool link = %q, want %s", got.PoolID, pool.ID)
	}
	var keptPool model.Pool
	if err := db.Write.First(&keptPool, "id = ?", pool.ID).Error; err != nil {
		t.Fatalf("pool lost in rebuild: %v", err)
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

func TestMigrateReplacesLegacyPoolBootstrapTokenConstraint(t *testing.T) {
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
		t.Fatalf("initial migrate: %v", err)
	}

	migrator := db.Write.Migrator()
	if err := migrator.DropConstraint("pool_bootstrap_tokens", "fk_pool_bootstrap_tokens_pool"); err != nil {
		t.Fatalf("remove current cascade constraint: %v", err)
	}
	if err := migrator.CreateConstraint(&legacyPoolConstraint{}, "BootstrapTokens"); err != nil {
		t.Fatalf("install legacy restrictive constraint: %v", err)
	}
	if !migrator.HasConstraint("pool_bootstrap_tokens", "fk_pools_bootstrap_tokens") {
		t.Fatal("legacy restrictive constraint was not installed")
	}

	project := &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}
	if err := db.Write.Create(project).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Type: "docker", Name: "Docker"}
	if err := db.Write.Create(provider).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	pool := &model.Pool{ID: "pool-1", ProjectID: project.ID, PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: provider.ID}}
	if err := db.Write.Create(pool).Error; err != nil {
		t.Fatalf("create pool: %v", err)
	}
	token := &model.PoolBootstrapToken{
		ID:        "token-1",
		PoolID:    pool.ID,
		TokenHash: []byte("token-hash"),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := db.Write.Create(token).Error; err != nil {
		t.Fatalf("create bootstrap token: %v", err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("upgrade migrate: %v", err)
	}
	if migrator.HasConstraint("pool_bootstrap_tokens", "fk_pools_bootstrap_tokens") {
		t.Fatal("legacy restrictive constraint remains after migration")
	}
	if !migrator.HasConstraint("pool_bootstrap_tokens", "fk_pool_bootstrap_tokens_pool") {
		t.Fatal("cascade constraint is missing after migration")
	}

	var tokenCount int64
	if err := db.Write.Model(&model.PoolBootstrapToken{}).Where("pool_id = ?", pool.ID).Count(&tokenCount).Error; err != nil {
		t.Fatalf("count preserved bootstrap tokens: %v", err)
	}
	if tokenCount != 1 {
		t.Fatalf("bootstrap token count after migration = %d, want 1", tokenCount)
	}
	if err := db.Write.Delete(pool).Error; err != nil {
		t.Fatalf("delete pool with bootstrap token: %v", err)
	}
	if err := db.Write.Model(&model.PoolBootstrapToken{}).Where("pool_id = ?", pool.ID).Count(&tokenCount).Error; err != nil {
		t.Fatalf("count cascaded bootstrap tokens: %v", err)
	}
	if tokenCount != 0 {
		t.Fatalf("bootstrap token count after pool delete = %d, want 0", tokenCount)
	}
}

// A database created before the slug column was retired still has it, declared
// NOT NULL with no default. AutoMigrate adds columns but never drops them, so
// without the migration every project write fails with a constraint violation —
// which is what a plain `sandbox restart` hit.
func TestMigrateDropsLegacyProjectSlugColumn(t *testing.T) {
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
		t.Fatalf("initial migrate: %v", err)
	}
	// Re-add the column the way AutoMigrate wrote it: NOT NULL, and quoted as
	// GORM quotes identifiers, so this is the schema a real upgrade meets.
	if err := db.Write.Exec("ALTER TABLE projects ADD COLUMN `slug` text NOT NULL DEFAULT ''").Error; err != nil {
		t.Fatalf("re-add legacy slug column: %v", err)
	}
	if !db.Write.Migrator().HasColumn(&model.Project{}, "slug") {
		t.Fatal("legacy slug column was not re-added")
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("upgrade migrate: %v", err)
	}

	if db.Write.Migrator().HasColumn(&model.Project{}, "slug") {
		t.Fatal("projects.slug survived the migration")
	}
	project := &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}
	if err := db.Write.Create(project).Error; err != nil {
		t.Fatalf("create project after migration: %v", err)
	}
	// The write path that failed in the field is an update, not just an insert.
	project.Name = "Renamed"
	if err := db.Write.Save(project).Error; err != nil {
		t.Fatalf("update project after migration: %v", err)
	}
}

type legacyPoolConstraint struct {
	ID              string                     `gorm:"column:id;primaryKey"`
	BootstrapTokens []model.PoolBootstrapToken `gorm:"foreignKey:PoolID;constraint:fk_pools_bootstrap_tokens"`
}

func (legacyPoolConstraint) TableName() string { return "pools" }

func fileExists(path string) bool {
	matches, err := filepath.Glob(path)
	return err == nil && len(matches) == 1
}
