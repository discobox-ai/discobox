package database_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/x/gormdb"
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

// legacySandboxSecret is the pre-ADR-0031 shape of the table: no
// agent_requested column, and a uniqueness index over (sandbox_id, env_name)
// alone. Migrating it is the upgrade path a real deployment takes, which a
// fresh database never exercises.
type legacySandboxSecret struct {
	ID        string    `gorm:"primaryKey;type:text"`
	ProjectID string    `gorm:"column:project_id;not null;type:text;index"`
	SandboxID string    `gorm:"column:sandbox_id;not null;type:text;index;uniqueIndex:idx_sandbox_secret_env,priority:1"`
	SecretID  string    `gorm:"column:secret_id;not null;type:text;index"`
	EnvName   string    `gorm:"column:env_name;not null;type:text;uniqueIndex:idx_sandbox_secret_env,priority:2"`
	Sentinel  string    `gorm:"column:sentinel;not null;type:text;uniqueIndex"`
	CreatedAt time.Time `gorm:"autoCreateTime"`
}

func (legacySandboxSecret) TableName() string { return "sandbox_secrets" }

// TestMigrateWidensSandboxSecretEnvIndex upgrades a database carrying the old
// narrow index and checks that a sandbox can then hold both an injected and an
// agent-requested binding for one environment variable.
//
// AutoMigrate creates a missing index but never alters one that already exists
// under the same name, so without the explicit drop the old index survives the
// upgrade and the second binding fails to insert — on a real deployment only,
// which is exactly the case a fresh-database test cannot see.
func TestMigrateWidensSandboxSecretEnvIndex(t *testing.T) {
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

	// Stand up the old schema, with a row in it, before upgrading.
	if err := db.Write.WithContext(ctx).AutoMigrate(&legacySandboxSecret{}); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&legacySandboxSecret{
		ID: "sbsec_old", ProjectID: "project-1", SandboxID: "sbx-1",
		SecretID: "sec-1", EnvName: "GITHUB_TOKEN", Sentinel: "STABLE-INJECTED",
	}).Error; err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if !db.Write.Migrator().HasIndex(&legacySandboxSecret{}, "idx_sandbox_secret_env") {
		t.Fatal("legacy schema did not create the narrow index; the test proves nothing")
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The pre-existing binding must survive the upgrade.
	var existing model.SandboxSecret
	if err := db.Write.WithContext(ctx).First(&existing, "id = ?", "sbsec_old").Error; err != nil {
		t.Fatalf("legacy row did not survive migration: %v", err)
	}
	if existing.AgentRequested {
		t.Fatal("an existing binding was upgraded into an agent-requested one")
	}

	// The same environment variable, on the other channel, must now fit.
	if err := db.Write.WithContext(ctx).Create(&model.SandboxSecret{
		ID: "sbsec_agent", ProjectID: "project-1", SandboxID: "sbx-1",
		SecretID: "sec-2", EnvName: "GITHUB_TOKEN", Sentinel: "STABLE-AGENT",
		AgentRequested: true,
	}).Error; err != nil {
		t.Fatalf("agent-requested binding rejected after migration: %v", err)
	}

	// The index must still do its job on the channel it governs.
	err = db.Write.WithContext(ctx).Create(&model.SandboxSecret{
		ID: "sbsec_dup", ProjectID: "project-1", SandboxID: "sbx-1",
		SecretID: "sec-3", EnvName: "GITHUB_TOKEN", Sentinel: "STABLE-DUP",
	}).Error
	if err == nil {
		t.Fatal("a second injected binding for the same env var was accepted; the index no longer constrains anything")
	}
}

// TestMigrateNormalizesSecretHosts repairs rows written before the secrets
// service normalized hosts. A grant whose host is not what the proxy reports is
// an approval nothing can match, so leaving old rows alone would leave the bug
// in place for exactly the deployments that hit it.
func TestMigrateNormalizesSecretHosts(t *testing.T) {
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

	if err := db.Write.WithContext(ctx).Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Written the way the pre-normalization service would have.
	if err := db.Write.WithContext(ctx).Exec(
		"INSERT INTO secret_grants (id, project_id, secret_id, scope, scope_key, host, granted_by, granted_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		"grant_shouty", "project-1", "sec-1", model.SecretGrantScopeSandbox, "sbx-1", "API.GitHub.com", "user-1",
		time.Now().UTC(), time.Now().UTC(), time.Now().UTC()).Error; err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var host string
	if err := db.Write.WithContext(ctx).Raw("SELECT host FROM secret_grants WHERE id = ?", "grant_shouty").Scan(&host).Error; err != nil {
		t.Fatalf("read grant: %v", err)
	}
	if host != "api.github.com" {
		t.Fatalf("host = %q, want it lowercased to what the proxy reports", host)
	}
}

// TestMigrateSecretTypesRenamesAndPrunes upgrades a database written against
// the four-type vocabulary. "bearer" becomes "token", and git and ssh secrets
// go with everything standing on them: they never resolved into a sandbox —
// cleartext leaves only through ResolveSandboxSecret, which emits the token —
// and a row left behind would fail the API's own enum on the way out, taking
// the whole secret listing with it.
func TestMigrateSecretTypesRenamesAndPrunes(t *testing.T) {
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
	if err := db.Write.WithContext(ctx).Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}

	now := time.Now().UTC()
	for _, row := range []struct{ id, kind string }{
		{"sec_bearer", "bearer"},
		{"sec_git", "git"},
		{"sec_ssh", "ssh"},
		{"sec_oauth", "oauth"},
	} {
		if err := db.Write.WithContext(ctx).Exec(
			"INSERT INTO secrets (id, project_id, name, type, max_grant_ttl_seconds, encrypted_value, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			row.id, "project-1", row.id, row.kind, 3600, []byte(`{"token":"x"}`), now, now).Error; err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}
	// A grant standing on one of the unusable secrets, which has to go with it.
	if err := db.Write.WithContext(ctx).Exec(
		"INSERT INTO secret_grants (id, project_id, secret_id, scope, scope_key, host, granted_by, granted_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		"grant_git", "project-1", "sec_git", model.SecretGrantScopeProject, "project-1", "github.com", "user-1", now, now, now).Error; err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var types []string
	if err := db.Write.WithContext(ctx).Raw("SELECT type FROM secrets ORDER BY id").Scan(&types).Error; err != nil {
		t.Fatalf("read secrets: %v", err)
	}
	if len(types) != 2 || types[0] != "token" || types[1] != "oauth" {
		t.Fatalf("types = %v, want the bearer row renamed and git/ssh gone", types)
	}
	var grants int64
	if err := db.Write.WithContext(ctx).Raw("SELECT count(*) FROM secret_grants").Scan(&grants).Error; err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if grants != 0 {
		t.Fatalf("grants = %d, want the one standing on a deleted secret gone", grants)
	}
}

// legacySecret is the pre-name shape of the uniqueness index: one secret per
// project, type and host. Widening it is the upgrade a real deployment takes.
type legacySecret struct {
	ID              string `gorm:"primaryKey;type:text"`
	ProjectID       string `gorm:"column:project_id;not null;type:text;index;uniqueIndex:idx_secret_project_type_host,priority:1"`
	Name            string `gorm:"column:name;not null;type:text"`
	Type            string `gorm:"column:type;not null;type:text;uniqueIndex:idx_secret_project_type_host,priority:2"`
	Host            string `gorm:"column:host;not null;type:text;default:'';uniqueIndex:idx_secret_project_type_host,priority:3"`
	UniqueKey       string `gorm:"column:unique_key;not null;type:text;default:'';uniqueIndex:idx_secret_project_type_host,priority:4"`
	Anonymous       bool   `gorm:"column:anonymous;not null;default:false;index"`
	Format          string `gorm:"column:format;not null;type:text;default:''"`
	DefaultGrantTTL int64  `gorm:"column:default_grant_ttl_seconds;not null;default:3600"`
	EncryptedValue  []byte `gorm:"column:encrypted_value"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (legacySecret) TableName() string { return "secrets" }

func TestMigrateWidensSecretUniquenessIndex(t *testing.T) {
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

	// The project too: the rebuild the index drop implies runs with foreign
	// keys on, and a secret pointing at a project that does not exist fails it.
	if err := db.Write.WithContext(ctx).AutoMigrate(&model.Project{}, &legacySecret{}); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&legacySecret{
		ID: "sec_old", ProjectID: "project-1", Name: "gh", Type: "token", EncryptedValue: []byte(`{"token":"a"}`),
	}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	// The state the old index forbade, which is now ordinary: a second unbound
	// token under another name.
	second := &legacySecret{ID: "sec_two", ProjectID: "project-1", Name: "openai", Type: "token", EncryptedValue: []byte(`{"token":"b"}`)}
	if err := db.Write.WithContext(ctx).Create(second).Error; err == nil {
		t.Fatal("the legacy schema accepted two unbound tokens; the test proves nothing")
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if err := db.Write.WithContext(ctx).Create(&model.Secret{
		ID: "sec_two", ProjectID: "project-1", Name: "openai", Type: "token", EncryptedValue: []byte(`{"token":"b"}`),
	}).Error; err != nil {
		t.Fatalf("a second unbound token was still refused after the upgrade: %v", err)
	}
	// And the index still does its job on the domain it governs.
	if err := db.Write.WithContext(ctx).Create(&model.Secret{
		ID: "sec_dup", ProjectID: "project-1", Name: "openai", Type: "token", EncryptedValue: []byte(`{"token":"c"}`),
	}).Error; err == nil {
		t.Fatal("the same name was accepted twice; the index no longer constrains anything")
	}
}

// A configured grant lifetime survives the rename that gave it teeth. The
// hazard the migration exists for is silent: AutoMigrate cannot tell a rename
// from an addition, so without it every secret would come back at the default
// with its real limit stranded in the old column.
func TestMigrateCarriesGrantTTLIntoTheLimitColumn(t *testing.T) {
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

	if err := db.Write.WithContext(ctx).AutoMigrate(&model.Project{}, &legacySecret{}); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&legacySecret{
		ID: "sec_day", ProjectID: "project-1", Name: "gh", Type: "token",
		DefaultGrantTTL: 86400, EncryptedValue: []byte(`{"token":"a"}`),
	}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var secret model.Secret
	if err := db.Write.WithContext(ctx).First(&secret, "id = ?", "sec_day").Error; err != nil {
		t.Fatalf("read secret: %v", err)
	}
	if secret.MaxGrantTTL != 86400 {
		t.Fatalf("limit = %d, want the 86400 the database already carried", secret.MaxGrantTTL)
	}
	if db.Write.Migrator().HasColumn(&model.Secret{}, "default_grant_ttl_seconds") {
		t.Fatal("the old column is still there; the value was copied rather than renamed")
	}

	// Idempotent: a second run finds nothing to rename and leaves the value be.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate twice: %v", err)
	}
	if err := db.Write.WithContext(ctx).First(&secret, "id = ?", "sec_day").Error; err != nil {
		t.Fatalf("read secret again: %v", err)
	}
	if secret.MaxGrantTTL != 86400 {
		t.Fatalf("limit after a second migrate = %d, want 86400", secret.MaxGrantTTL)
	}
}

// A harness's own credential comes out of the migration with no ceiling. Its
// grant never expires, so a limit on it would describe a lifetime that grant
// does not have — and the hour these rows carry was the old column default,
// never a number anybody chose.
func TestMigrateLiftsTheLimitOnConfiguredHarnessSecrets(t *testing.T) {
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
	if err := db.Write.WithContext(ctx).Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Three secrets in the same project: the one a configure flow made, one a
	// person made and bound to the same harness, and one somebody deliberately
	// gave a limit to after it was configured.
	configured := &model.Secret{ID: "sec_configured", ProjectID: "project-1", Name: "claude", Type: model.SecretTypeToken, UniqueKey: "sec_configured", MaxGrantTTL: 3600, EncryptedValue: []byte(`{"token":"a"}`)}
	byHand := &model.Secret{ID: "sec_byhand", ProjectID: "project-1", Name: "mine", Type: model.SecretTypeToken, MaxGrantTTL: 3600, EncryptedValue: []byte(`{"token":"b"}`)}
	chosen := &model.Secret{ID: "sec_chosen", ProjectID: "project-1", Name: "chosen", Type: model.SecretTypeToken, UniqueKey: "sec_chosen", MaxGrantTTL: 900, EncryptedValue: []byte(`{"token":"c"}`)}
	for _, secret := range []*model.Secret{configured, byHand, chosen} {
		if err := db.Write.WithContext(ctx).Create(secret).Error; err != nil {
			t.Fatalf("seed %s: %v", secret.ID, err)
		}
	}
	if err := db.Write.WithContext(ctx).Create(&model.HarnessConfig{
		ProjectID: "project-1", Slug: "claude-code", Name: "Claude Code", Image: "img:1",
		Configured: true, ConfiguredSecretIDs: []string{configured.ID, chosen.ID},
	}).Error; err != nil {
		t.Fatalf("seed harness config: %v", err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	limits := map[string]int64{}
	var secrets []model.Secret
	if err := db.Write.WithContext(ctx).Find(&secrets).Error; err != nil {
		t.Fatalf("read secrets: %v", err)
	}
	for _, secret := range secrets {
		limits[secret.ID] = secret.MaxGrantTTL
	}
	if limits["sec_configured"] != 0 {
		t.Fatalf("configured secret limit = %d, want none", limits["sec_configured"])
	}
	if limits["sec_byhand"] != 3600 {
		t.Fatalf("hand-made secret limit = %d, want the 3600 it had: nothing configured it", limits["sec_byhand"])
	}
	if limits["sec_chosen"] != 900 {
		t.Fatalf("deliberately limited secret = %d, want the 900 somebody set", limits["sec_chosen"])
	}
}
