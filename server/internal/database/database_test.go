package database_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/gormdb"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
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

// A Mac installed before the vz backend existed carries a disabled provider of
// type "macos" — a placeholder that was never a registered provider type — and
// a Default pool bound to it. The upgrade must adopt that instance rather than
// leave it inert, or the pool a fresh install was given can never start.
func TestMigrateAdoptsThePlaceholderMacOSProvider(t *testing.T) {
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
		Type:      "macos",
		Name:      "macOS",
		Disabled:  true,
	}
	if err := db.Write.Create(provider).Error; err != nil {
		t.Fatalf("create placeholder provider: %v", err)
	}
	pool := &model.Pool{ID: "pool-1", ProjectID: project.ID, PoolManifest: model.PoolManifest{Name: "Default", ProviderInstanceID: provider.ID}}
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
	if got.Type != "vz" {
		t.Fatalf("provider type = %q, want vz", got.Type)
	}
	// The placeholder was disabled only because it could not run anything.
	if got.Disabled {
		t.Fatal("adopted provider is still disabled")
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
	// Re-add the column and its unique index the way AutoMigrate wrote them
	// (NOT NULL, and a uniqueIndex struct tag), so this is the schema a real
	// upgrade meets — including the index that a plain ALTER TABLE DROP
	// COLUMN refuses to drop past.
	if err := db.Write.Exec("ALTER TABLE projects ADD COLUMN `slug` text NOT NULL DEFAULT ''").Error; err != nil {
		t.Fatalf("re-add legacy slug column: %v", err)
	}
	if err := db.Write.Exec("CREATE UNIQUE INDEX `idx_projects_slug` ON `projects`(`slug`)").Error; err != nil {
		t.Fatalf("re-add legacy slug index: %v", err)
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

// TestMigrateDeduplicatesSandboxNames reproduces upgrading a database written
// before sandbox names were unique. The unique index cannot be created over
// duplicates, and a duplicate must not be able to strand a server on startup,
// so the migration renames the later holders instead of failing.
func TestMigrateDeduplicatesSandboxNames(t *testing.T) {
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
	other := &model.Project{ID: "project-2", OwnerUserID: "user-1", Name: "Other"}
	for _, p := range []*model.Project{project, other} {
		if err := db.Write.Create(p).Error; err != nil {
			t.Fatalf("create project: %v", err)
		}
	}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Type: "docker", Name: "Docker"}
	if err := db.Write.Create(provider).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	pool := &model.Pool{ID: "pool-1", ProjectID: project.ID, PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: provider.ID}}
	if err := db.Write.Create(pool).Error; err != nil {
		t.Fatalf("create pool: %v", err)
	}

	// Drop the index to get the pre-uniqueness schema back, which is the only
	// way duplicates can be written at all.
	if err := db.Write.Migrator().DropIndex(&model.Sandbox{}, "idx_sandbox_project_name"); err != nil {
		t.Fatalf("drop unique index: %v", err)
	}
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, sandbox := range []*model.Sandbox{
		{ID: "sbx_oldest", ProjectID: project.ID, PoolID: pool.ID, CreatedByUserID: "user-1", Name: "twin"},
		{ID: "sbx_middle", ProjectID: project.ID, PoolID: pool.ID, CreatedByUserID: "user-1", Name: "twin"},
		{ID: "sbx_newest", ProjectID: project.ID, PoolID: pool.ID, CreatedByUserID: "user-1", Name: "twin"},
		{ID: "sbx_unique", ProjectID: project.ID, PoolID: pool.ID, CreatedByUserID: "user-1", Name: "solo"},
		// Same name, different project: uniqueness is project-scoped.
		{ID: "sbx_other0", ProjectID: other.ID, PoolID: pool.ID, CreatedByUserID: "user-1", Name: "twin"},
	} {
		sandbox.CreatedAt = created.Add(time.Duration(i) * time.Minute)
		if err := db.Write.Create(sandbox).Error; err != nil {
			t.Fatalf("create sandbox %s: %v", sandbox.ID, err)
		}
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("upgrade migrate: %v", err)
	}

	names := map[string]string{}
	var sandboxes []model.Sandbox
	if err := db.Write.Find(&sandboxes).Error; err != nil {
		t.Fatalf("read sandboxes: %v", err)
	}
	for _, sandbox := range sandboxes {
		names[sandbox.ID] = sandbox.Name
	}
	// The oldest holder keeps the name the user has been using.
	if names["sbx_oldest"] != "twin" {
		t.Fatalf("oldest sandbox name = %q, want twin", names["sbx_oldest"])
	}
	if names["sbx_unique"] != "solo" {
		t.Fatalf("an unaffected sandbox was renamed to %q", names["sbx_unique"])
	}
	if names["sbx_other0"] != "twin" {
		t.Fatalf("a same-name sandbox in another project was renamed to %q", names["sbx_other0"])
	}
	for _, id := range []string{"sbx_middle", "sbx_newest"} {
		if names[id] == "twin" {
			t.Fatalf("%s kept the duplicate name", id)
		}
		if !strings.Contains(names[id], id) {
			t.Fatalf("%s renamed to %q, which does not identify the sandbox", id, names[id])
		}
	}

	// The whole point: the index exists now, and rejects a new duplicate.
	dup := &model.Sandbox{ID: "sbx_late00", ProjectID: project.ID, PoolID: pool.ID, CreatedByUserID: "user-1", Name: "twin"}
	if err := db.Write.Create(dup).Error; err == nil {
		t.Fatal("expected the unique index to reject a duplicate sandbox name")
	}
}

// A pre-ADR-0034 sandbox row carries a power value in `state` and the provider
// blob in `runtime_state`, which the split repurposed for the runtime axis.
// Both have to land on the right field, and the pass has to be safe to repeat —
// a second run must not mistake the runtime state it just wrote for a blob, or
// re-migrate a row it already moved.
func TestMigrateSplitsLegacySandboxState(t *testing.T) {
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
	reshapeSandboxesToPreSplitSchema(t, db)

	project := &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}
	if err := db.Write.Create(project).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Type: "test", Name: "Test"}
	if err := db.Write.Create(provider).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	pool := &model.Pool{ID: "pool-1", ProjectID: project.ID, PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: provider.ID}}
	if err := db.Write.Create(pool).Error; err != nil {
		t.Fatalf("create pool: %v", err)
	}

	// Rows as the old schema wrote them, including one whose provider never
	// recorded any state and so left the column NULL.
	legacy := []struct {
		id           string
		state        string
		runtimeState any
	}{
		// The blob goes in as []byte, which is how GORM wrote a
		// json.RawMessage field and therefore what storage class the value
		// really has in an upgraded database.
		{id: "sandbox-running", state: "running", runtimeState: []byte(`{"id":"runtime-1"}`)},
		{id: "sandbox-stopped", state: "stopped", runtimeState: []byte(`{"id":"runtime-2"}`)},
		{id: "sandbox-awaiting", state: "awaiting_source", runtimeState: nil},
		{id: "sandbox-archived", state: "archived", runtimeState: []byte(`{"id":"runtime-3"}`)},
	}
	for _, row := range legacy {
		if err := db.Write.Exec(
			`INSERT INTO sandboxes (id, project_id, created_by_user_id, pool_id, name, desired_state, state, generation, observed_generation, runtime_state, created_at, updated_at)
			 VALUES (?, ?, 'user-1', ?, ?, 'present', ?, 1, 1, ?, ?, ?)`,
			row.id, project.ID, pool.ID, row.id, row.state, row.runtimeState, time.Now(), time.Now()).Error; err != nil {
			t.Fatalf("insert legacy row %s: %v", row.id, err)
		}
	}

	// Twice: the migration must be idempotent, because an interrupted upgrade
	// re-runs it against rows it already moved.
	for pass := 1; pass <= 2; pass++ {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("pass %d: upgrade migrate: %v", pass, err)
		}
		for _, want := range []struct {
			id            string
			state         string
			runtimeState  string
			providerState string
		}{
			{id: "sandbox-running", state: "ready", runtimeState: "running", providerState: `{"id":"runtime-1"}`},
			{id: "sandbox-stopped", state: "ready", runtimeState: "stopped", providerState: `{"id":"runtime-2"}`},
			// An existence state is already on the right field and stays put,
			// with nothing observed about its runtime.
			{id: "sandbox-awaiting", state: "awaiting_source", runtimeState: "", providerState: ""},
			{id: "sandbox-archived", state: "archived", runtimeState: "", providerState: `{"id":"runtime-3"}`},
		} {
			var got model.Sandbox
			if err := db.Write.First(&got, "id = ?", want.id).Error; err != nil {
				t.Fatalf("pass %d: load %s: %v", pass, want.id, err)
			}
			if got.State != want.state {
				t.Errorf("pass %d: %s state = %q, want %q", pass, want.id, got.State, want.state)
			}
			if got.RuntimeState != want.runtimeState {
				t.Errorf("pass %d: %s runtime state = %q, want %q", pass, want.id, got.RuntimeState, want.runtimeState)
			}
			if string(got.ProviderState) != want.providerState {
				t.Errorf("pass %d: %s provider state = %q, want %q", pass, want.id, got.ProviderState, want.providerState)
			}
		}
	}
}

// reshapeSandboxesToPreSplitSchema rewinds the sandboxes table to what a
// database created before ADR 0034 actually holds: no provider_state, and a
// nullable runtime_state carrying the provider blob. Migrating a table the
// current schema just created would prove nothing — its runtime_state is
// already NOT NULL, so the NULL a real upgrade meets could not even be written.
//
// The foreign-key pragma comes off around the rebuild for the reason
// dropRetiredColumn documents: SQLite drops a column by rebuilding the table,
// and sandboxes are referenced from elsewhere in the schema.
func reshapeSandboxesToPreSplitSchema(t *testing.T, db *database.DB) {
	t.Helper()
	err := db.Write.Connection(func(tx *gorm.DB) error {
		if err := tx.Exec("PRAGMA foreign_keys = OFF").Error; err != nil {
			return err
		}
		defer tx.Exec("PRAGMA foreign_keys = ON")
		for _, statement := range []string{
			"ALTER TABLE sandboxes DROP COLUMN `provider_state`",
			"ALTER TABLE sandboxes DROP COLUMN `runtime_state_changed_at`",
			"ALTER TABLE sandboxes DROP COLUMN `runtime_state`",
			"ALTER TABLE sandboxes ADD COLUMN `runtime_state` text",
		} {
			if err := tx.Exec(statement).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reshape sandboxes to the pre-split schema: %v", err)
	}
}
