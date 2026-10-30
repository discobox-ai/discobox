package database_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"text/template"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/internal/originkey"
	"github.com/discobox-ai/discobox/pkg/platform"
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

	for _, table := range []string{"users", "projects", "sandboxes", "sandbox_provider_instances", "pools"} {
		if !db.Write.Migrator().HasTable(table) {
			t.Fatalf("schema missing table %s", table)
		}
	}
	for _, table := range []string{"organizations", "project_events"} {
		if db.Write.Migrator().HasTable(table) {
			t.Fatalf("schema unexpectedly has %s table", table)
		}
	}
}

// A database from before ADR 0081 has a project_events table, with rows and a
// foreign key onto projects. Migrating drops it; the projects it referenced
// survive, because the reference only ever ran one way.
func TestMigrateDropsProjectEvents(t *testing.T) {
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
	for _, stmt := range []string{
		"CREATE TABLE project_events (id text, seq integer PRIMARY KEY AUTOINCREMENT, project_id text NOT NULL, " +
			"type text NOT NULL, resource_type text NOT NULL, resource_id text NOT NULL, action text NOT NULL, " +
			"data text NOT NULL, created_at datetime, CONSTRAINT fk_project_events_project " +
			"FOREIGN KEY (project_id) REFERENCES projects(id))",
		"CREATE INDEX idx_project_event_seq ON project_events(project_id, created_at)",
		"INSERT INTO project_events (id, project_id, type, resource_type, resource_id, action, data, created_at) " +
			"VALUES ('evt_1', 'project-1', 'resource.changed', 'sandbox', 'sandbox-1', 'created', '{}', '2026-01-01 00:00:00')",
	} {
		if err := db.Write.Exec(stmt).Error; err != nil {
			t.Fatalf("recreate legacy project_events %q: %v", stmt, err)
		}
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("upgrade migrate: %v", err)
	}
	if db.Write.Migrator().HasTable("project_events") {
		t.Fatal("project_events still exists after migration")
	}
	if err := db.Write.First(&model.Project{}, "id = ?", project.ID).Error; err != nil {
		t.Fatalf("project lost with its events: %v", err)
	}
	// Migrating again must be a no-op rather than an error on the missing table.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate: %v", err)
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

// A database from before pools were enrolled by their maker has the pool
// bootstrap token table, and its tokens; the migration drops it whole, and
// leaves the pools (ADR 26-10-24-953 §4).
func TestMigrateDropsThePoolBootstrapTokenTable(t *testing.T) {
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
	if err := db.Write.Exec("CREATE TABLE `pool_bootstrap_tokens` (`id` text PRIMARY KEY, `pool_id` text NOT NULL, `token_hash` blob NOT NULL)").Error; err != nil {
		t.Fatalf("re-create the legacy table: %v", err)
	}
	if err := db.Write.Exec("INSERT INTO `pool_bootstrap_tokens` (`id`, `pool_id`, `token_hash`) VALUES ('pbt-1', 'pool-1', x'00')").Error; err != nil {
		t.Fatalf("insert a legacy token: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("upgrade migrate: %v", err)
	}
	if db.Write.Migrator().HasTable("pool_bootstrap_tokens") {
		t.Fatal("pool_bootstrap_tokens survived the migration")
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate again: %v", err)
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

// TestMigrateMovesSecretHostsToLists upgrades requests and grants written when
// each carried one host (ADR 26-10-02-393 §6): the host becomes the only one
// in the list, normalized the way the proxy reports a destination, and the
// retired column is gone. A grant whose host is not what the proxy reports is
// an approval nothing can match, so the casing old rows were written in is
// repaired on the way — and carried into the binding the grant becomes (ADR
// 26-10-27-880 §8).
func TestMigrateMovesSecretHostsToLists(t *testing.T) {
	ctx := context.Background()
	db := openGrantEra(t)
	write := db.Write.WithContext(ctx)
	// The column as a database written before the list had it.
	for _, table := range []string{"secret_requests", "secret_grants"} {
		if err := write.Exec("ALTER TABLE " + table + " ADD COLUMN host text NOT NULL DEFAULT ''").Error; err != nil {
			t.Fatalf("restore %s.host: %v", table, err)
		}
	}
	if err := write.Create(&model.Sandbox{ID: "sbx-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "one"}).Error; err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	now := time.Now().UTC()
	for _, stmt := range []string{
		"INSERT INTO secrets (id, project_id, name, type, max_grant_ttl_seconds) VALUES ('sec-1', 'project-1', 'github', 'token', 0)",
		"INSERT INTO sandbox_secrets (id, project_id, sandbox_id, secret_id, env_name, sentinel) VALUES ('ss_1', 'project-1', 'sbx-1', 'sec-1', 'GH_TOKEN', 'SENTINEL')",
		"INSERT INTO secret_grants (id, project_id, secret_id, scope, scope_key, host) VALUES ('grant_shouty', 'project-1', 'sec-1', 'project', 'project-1', 'API.GitHub.com')",
	} {
		if err := write.Exec(stmt).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := write.Exec(
		"INSERT INTO secret_requests (id, project_id, requested_by, sandbox_id, name, env_name, type, host, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		"req_old", "project-1", "agent:sbx-1", "sbx-1", "github", "GH_TOKEN", model.SecretTypeToken, "api.github.com", model.SecretRequestStatusPending, now, now).Error; err != nil {
		t.Fatalf("seed request: %v", err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Twice: the step runs on every start, and the second finds nothing to do.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate again: %v", err)
	}

	if db.Write.Migrator().HasColumn("secret_requests", "host") {
		t.Errorf("secret_requests.host survived the migration")
	}
	var binding model.SecretBinding
	if err := write.First(&binding, "id = ?", "bind_1").Error; err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if !reflect.DeepEqual(binding.Routes, []model.SecretRoute{upgraded("api.github.com", "", false)}) {
		t.Errorf("routes = %+v, want [api.github.com], lowercased to what the proxy reports", binding.Routes)
	}
	var req model.SecretRequest
	if err := write.First(&req, "id = ?", "req_old").Error; err != nil {
		t.Fatalf("read request: %v", err)
	}
	if !reflect.DeepEqual(req.Routes, []model.SecretRoute{upgraded("api.github.com", "", false)}) {
		t.Errorf("request routes = %+v, want [api.github.com]", req.Routes)
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
	grantEraSchema(t, db)
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
		"INSERT INTO secret_grants (id, project_id, secret_id, scope, scope_key, hosts, granted_by, granted_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		"grant_git", "project-1", "sec_git", "project", "project-1", `["github.com"]`, "user-1", now, now, now).Error; err != nil {
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
	var bindings int64
	if err := db.Write.WithContext(ctx).Model(&model.SecretBinding{}).Count(&bindings).Error; err != nil {
		t.Fatalf("count bindings: %v", err)
	}
	if bindings != 0 {
		t.Fatalf("bindings = %d, want nothing made of the grant standing on a deleted secret", bindings)
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
	if secret.MaxBindingTTL != 86400 {
		t.Fatalf("limit = %d, want the 86400 the database already carried", secret.MaxBindingTTL)
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
	if secret.MaxBindingTTL != 86400 {
		t.Fatalf("limit after a second migrate = %d, want 86400", secret.MaxBindingTTL)
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
	configured := &model.Secret{ID: "sec_configured", ProjectID: "project-1", Name: "claude", Type: model.SecretTypeToken, MaxBindingTTL: 3600, EncryptedValue: []byte(`{"token":"a"}`)}
	byHand := &model.Secret{ID: "sec_byhand", ProjectID: "project-1", Name: "mine", Type: model.SecretTypeToken, MaxBindingTTL: 3600, EncryptedValue: []byte(`{"token":"b"}`)}
	chosen := &model.Secret{ID: "sec_chosen", ProjectID: "project-1", Name: "chosen", Type: model.SecretTypeToken, MaxBindingTTL: 900, EncryptedValue: []byte(`{"token":"c"}`)}
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
		limits[secret.ID] = secret.MaxBindingTTL
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

// A codex config.toml captured before trust followed the working directory
// overlays the image's fixed one, so a sourceless sandbox on that config trusts
// nothing. Migrate moves the stanza it carries onto workingDir, falling back to
// the primary source's target on an agent that sets no workingDir, wherever the
// configure flow put it, and leaves what somebody rewrote by hand, and every
// other harness's files, as they were.
func TestMigrateRetrustsCodexConfiguredWorkingDir(t *testing.T) {
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

	// As configure.sh wrote them: settings, a newline, then the stanza — or the
	// stanza alone, its leading newline stripped, when there were no settings.
	const oldStanza = "\n{{- range .sources }}{{- if eq .slug \"primary\" }}\n[projects.{{ .target | json }}]\ntrust_level = \"trusted\"\n{{- end }}{{- end }}\n"
	const newStanza = "\n{{- if .workingDir }}\n[projects.{{ .workingDir | json }}]\ntrust_level = \"trusted\"\n{{- else }}" +
		"{{- range .sources }}{{- if eq .slug \"primary\" }}\n[projects.{{ .target | json }}]\ntrust_level = \"trusted\"\n{{- end }}{{- end }}" +
		"{{- end }}\n"
	const settings = "model = \"gpt-5\"\n"
	const handWritten = "model = \"gpt-5\"\n\n[projects.\"/src\"]\ntrust_level = \"trusted\"\n"
	auth := model.HarnessConfigFile{Path: ".codex/auth.json", Template: true, Content: `{"OPENAI_API_KEY": "{{ .secrets.OPENAI_API_KEY }}"}`}
	stamp := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	seeds := map[string][]model.HarnessConfigFile{
		"codex-settings": {{Path: ".codex/config.toml", Template: true, Content: settings + oldStanza}, auth},
		"codex-bare":     {{Path: ".codex/config.toml", Template: true, Content: strings.TrimLeft(oldStanza, "\n")}},
		"codex-by-hand":  {{Path: ".codex/config.toml", Template: true, Content: handWritten}},
		"elsewhere":      {{Path: ".other/config.toml", Template: true, Content: settings + oldStanza}},
	}
	for slug, files := range seeds {
		if err := db.Write.WithContext(ctx).Create(&model.HarnessConfig{
			ID: "hc_" + slug, ProjectID: "project-1", Slug: slug, Name: slug, Image: "img:1",
			Configured: true, ConfiguredFiles: files, UpdatedAt: stamp,
		}).Error; err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
		if err := db.Write.WithContext(ctx).Model(&model.HarnessConfig{ID: "hc_" + slug}).UpdateColumn("updated_at", stamp).Error; err != nil {
			t.Fatalf("stamp %s: %v", slug, err)
		}
	}

	want := map[string][]model.HarnessConfigFile{
		"codex-settings": {{Path: ".codex/config.toml", Template: true, Content: settings + newStanza}, auth},
		"codex-bare":     {{Path: ".codex/config.toml", Template: true, Content: strings.TrimLeft(newStanza, "\n")}},
		"codex-by-hand":  seeds["codex-by-hand"],
		"elsewhere":      seeds["elsewhere"],
	}
	// Twice: it runs on every start, and a second run finds nothing to do.
	for range 2 {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		for slug, files := range want {
			var got model.HarnessConfig
			if err := db.Write.WithContext(ctx).First(&got, "id = ?", "hc_"+slug).Error; err != nil {
				t.Fatalf("read %s: %v", slug, err)
			}
			if !reflect.DeepEqual(got.ConfiguredFiles, files) {
				t.Fatalf("%s configured files = %#v, want %#v", slug, got.ConfiguredFiles, files)
			}
			if !got.UpdatedAt.Equal(stamp) {
				t.Fatalf("%s updated_at = %v, want the %v it had: a migration is not an edit", slug, got.UpdatedAt, stamp)
			}
		}
	}

	// What the migrated file renders to, the way sandbox-agent renders a harness
	// file: the stored config may name an image whose agent predates workingDir,
	// and there it must trust what the old stanza did rather than nothing.
	primary := []any{map[string]any{"slug": "primary", "target": "/src"}}
	renders := []struct {
		name    string
		context map[string]any
		want    string
	}{
		{"current agent, no source", map[string]any{"workingDir": "/home/discobox", "sources": []any{}},
			settings + "[projects.\"/home/discobox\"]\ntrust_level = \"trusted\"\n"},
		{"current agent, source", map[string]any{"workingDir": "/src/app", "sources": primary},
			settings + "[projects.\"/src/app\"]\ntrust_level = \"trusted\"\n"},
		{"older agent, source", map[string]any{"sources": primary},
			settings + "[projects.\"/src\"]\ntrust_level = \"trusted\"\n"},
		{"older agent, no source", map[string]any{"sources": []any{}}, settings},
	}
	for _, render := range renders {
		tmpl, err := template.New(render.name).
			Funcs(template.FuncMap{"json": func(value any) (string, error) {
				encoded, err := json.Marshal(value)
				return string(encoded), err
			}}).
			Option("missingkey=zero").
			Parse(want["codex-settings"][0].Content)
		if err != nil {
			t.Fatalf("%s: parse: %v", render.name, err)
		}
		var rendered strings.Builder
		if err := tmpl.Execute(&rendered, render.context); err != nil {
			t.Fatalf("%s: render: %v", render.name, err)
		}
		if rendered.String() != render.want {
			t.Fatalf("%s rendered %q, want %q", render.name, rendered.String(), render.want)
		}
	}
}

// ADR 0111 files a sandbox under its origin host and where its primary source
// came from, or its host alone when it has no source. An upgraded database's
// rows are re-keyed from what they recorded: a local-sourced row's key does not
// move, a remote-sourced one moves from the directory it was created in to its
// URL, and a sourceless one to its host. The project path an older server
// stored stays in the origin, unread, and nothing here counts as an update.
func TestMigrateRekeysSandboxOrigins(t *testing.T) {
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
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Type: "docker", Name: "Docker"}
	if err := db.Write.Create(provider).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	pool := &model.Pool{ID: "pool-1", ProjectID: project.ID, PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: provider.ID}}
	if err := db.Write.Create(pool).Error; err != nil {
		t.Fatalf("create pool: %v", err)
	}

	const host = "host_aaaaaaaaaaaaaaaa"
	dir, url := "/src/alpha", "https://github.com/acme/api"
	local := originkey.Of(host, dir)
	// Where an older server filed the two that did not run in their source:
	// the directory the create happened to run in.
	scratch := originkey.Of(host, "/scratch")
	updated := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, sandbox := range []*model.Sandbox{
		{ID: "sbx_local", Name: "local", OriginKey: &local,
			SandboxManifest: model.SandboxManifest{Source: &model.GitSource{Kind: "git", LocalDirectory: &dir}}},
		{ID: "sbx_remote", Name: "remote", OriginKey: &scratch,
			SandboxManifest: model.SandboxManifest{Source: &model.GitSource{Kind: "git", URL: &url}}},
		{ID: "sbx_none", Name: "none", OriginKey: &scratch},
	} {
		sandbox.ProjectID, sandbox.PoolID, sandbox.CreatedByUserID = project.ID, pool.ID, "user-1"
		sandbox.Origin = &model.Origin{HostID: host}
		sandbox.UpdatedAt = updated
		if err := db.Write.Create(sandbox).Error; err != nil {
			t.Fatalf("create sandbox %s: %v", sandbox.ID, err)
		}
	}
	// What an older server wrote into the origin: the project path as well.
	legacy := `{"hostId":"` + host + `","projectPath":"/scratch"}`
	if err := db.Write.Exec("UPDATE sandboxes SET origin = ?", legacy).Error; err != nil {
		t.Fatalf("write legacy origins: %v", err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("upgrade migrate: %v", err)
	}

	want := map[string]string{
		"sbx_local":  local,
		"sbx_remote": originkey.Of(host, url),
		"sbx_none":   originkey.Host(host),
	}
	var sandboxes []model.Sandbox
	if err := db.Write.Find(&sandboxes).Error; err != nil {
		t.Fatalf("read sandboxes: %v", err)
	}
	if len(sandboxes) != len(want) {
		t.Fatalf("read %d sandboxes, want %d", len(sandboxes), len(want))
	}
	for _, sandbox := range sandboxes {
		if sandbox.OriginKey == nil || *sandbox.OriginKey != want[sandbox.ID] {
			t.Errorf("%s: origin key = %v, want %q", sandbox.ID, sandbox.OriginKey, want[sandbox.ID])
		}
		if sandbox.Origin == nil || sandbox.Origin.HostID != host {
			t.Errorf("%s: origin = %+v, want it still decoded with its host", sandbox.ID, sandbox.Origin)
		}
		if !sandbox.UpdatedAt.Equal(updated) {
			t.Errorf("%s: updated_at = %v, want it untouched at %v", sandbox.ID, sandbox.UpdatedAt, updated)
		}
	}
}

func TestMigrateRetiresPrepullWithoutLosingPools(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{Driver: gormdb.DriverSQLite, DSN: "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		"ALTER TABLE pools ADD COLUMN images_staged boolean NOT NULL DEFAULT false",
		"ALTER TABLE pools ADD COLUMN image_stage text",
		"ALTER TABLE pools ADD COLUMN image_staged_at datetime",
		"INSERT INTO reconcile_dirty (resource_type, resource_id, seq, not_before, attempts, marked_at, created_at, updated_at) VALUES ('poolImages', 'pool-1', 1, CURRENT_TIMESTAMP, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP), ('pool', 'project-1/pool-1', 1, CURRENT_TIMESTAMP, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)",
	} {
		if err := db.Write.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	// Foreign keys require the ordinary parent rows too.
	project := model.Project{ID: "project-1", Name: "project"}
	if err := db.Write.Create(&project).Error; err != nil {
		t.Fatal(err)
	}
	provider := model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Name: "provider", Type: "docker"}
	if err := db.Write.Create(&provider).Error; err != nil {
		t.Fatal(err)
	}
	pool := model.Pool{ID: "pool-1", ProjectID: project.ID, PoolManifest: model.PoolManifest{ProviderInstanceID: provider.ID, Name: "pool"}}
	if err := db.Write.Create(&pool).Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var got model.Pool
	if err := db.Write.First(&got, "id = ?", pool.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.ProviderInstanceID != provider.ID || got.DesiredState != pool.DesiredState {
		t.Fatalf("pool changed: %+v", got)
	}
	for _, column := range []string{"images_staged", "image_stage", "image_staged_at"} {
		if db.Write.Migrator().HasColumn(&model.Pool{}, column) {
			t.Fatalf("retired column remains: %s", column)
		}
	}
	var types []string
	if err := db.Write.Table("reconcile_dirty").Pluck("resource_type", &types).Error; err != nil {
		t.Fatal(err)
	}
	if len(types) != 1 || types[0] != "pool" {
		t.Fatalf("dirty work = %v, want only pool", types)
	}
}

// A secret written before value_updated_at existed gets the time its value was
// last written, so its lifetime does not restart on the next rename.
func TestMigrateBackfillsSecretValueUpdatedAt(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{
		Driver: gormdb.DriverSQLite,
		DSN:    "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db"),
	})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}
	if err := db.Write.Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	// Written the way older rows were: in the server's local zone, nine hours
	// east, which SQLite keeps as text with its offset.
	written := time.Date(2025, 9, 1, 21, 0, 0, 0, time.FixedZone("UTC+9", 9*60*60))
	if err := db.Write.Exec(`INSERT INTO secrets (id, project_id, name, type, anonymous, format, max_binding_ttl_seconds, ttl_seconds, created_at, updated_at)
		VALUES ('sec_legacy', 'project-1', 'github', 'token', false, '', 3600, 0, ?, ?)`, written, written).Error; err != nil {
		t.Fatalf("insert legacy secret: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var secret model.Secret
	if err := db.Write.First(&secret, "id = ?", "sec_legacy").Error; err != nil {
		t.Fatal(err)
	}
	if secret.ValueUpdatedAt == nil || !secret.ValueUpdatedAt.Equal(written) {
		t.Fatalf("value_updated_at = %v, want the row's last write %v", secret.ValueUpdatedAt, written)
	}
	// In UTC, because it is compared as text against UTC times in SQL.
	var text string
	if err := db.Write.Raw("SELECT CAST(value_updated_at AS TEXT) FROM secrets WHERE id = ?", "sec_legacy").Scan(&text).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(text, "+00:00") && !strings.HasSuffix(text, "Z") {
		t.Fatalf("value_updated_at stored as %q, want UTC", text)
	}
}

// A database from before platforms were recorded gains the columns on every
// table that places by them, and no row is given a guess (ADR 0145 §1). A
// built-in harness is re-inspected at the next seed and records what its image
// publishes; any other harness reads as unknown, which rules nothing out, until
// it is refreshed. A pool's agent declares its platform on its next report,
// and a sandbox takes its pool's when it is next placed. A guess would be
// wrong for a pool of another architecture, and a sandbox's platform is
// never corrected once written.
func TestMigrateGuessesNoPlatform(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{Driver: gormdb.DriverSQLite, DSN: "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	project := model.Project{ID: "project-1", Name: "project"}
	if err := db.Write.Create(&project).Error; err != nil {
		t.Fatal(err)
	}
	provider := model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Name: "provider", Type: "docker"}
	if err := db.Write.Create(&provider).Error; err != nil {
		t.Fatal(err)
	}
	registeredAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pool := model.Pool{ID: "pool-1", ProjectID: project.ID, RegisteredAt: &registeredAt, PoolManifest: model.PoolManifest{ProviderInstanceID: provider.ID, Name: "pool"}}
	if err := db.Write.Create(&pool).Error; err != nil {
		t.Fatal(err)
	}
	config := model.HarnessConfig{ID: "hc-1", ProjectID: project.ID, Slug: "shell", Name: "Shell"}
	if err := db.Write.Create(&config).Error; err != nil {
		t.Fatal(err)
	}
	sandbox := model.Sandbox{ID: "sbx-1", ProjectID: project.ID, PoolID: pool.ID, Name: "one", CreatedByUserID: "user-1"}
	if err := db.Write.Create(&sandbox).Error; err != nil {
		t.Fatal(err)
	}
	// The schema those rows were written under: no platform anywhere.
	for table, column := range map[string]string{"pools": "platform", "harness_configs": "platforms", "sandboxes": "platform"} {
		if err := db.Write.Exec("ALTER TABLE " + table + " DROP COLUMN " + column).Error; err != nil {
			t.Fatalf("drop %s.%s: %v", table, column, err)
		}
	}

	for range 2 {
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}

	var gotConfig model.HarnessConfig
	if err := db.Write.First(&gotConfig, "id = ?", config.ID).Error; err != nil {
		t.Fatal(err)
	}
	if len(gotConfig.Platforms) != 0 {
		t.Errorf("harness config platforms = %q, want none until its image is inspected", gotConfig.Platforms)
	}
	var gotPool model.Pool
	if err := db.Write.First(&gotPool, "id = ?", pool.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !gotPool.Platform.IsZero() {
		t.Errorf("pool platform = %q, want it left for its agent to declare", gotPool.Platform)
	}
	var gotSandbox model.Sandbox
	if err := db.Write.First(&gotSandbox, "id = ?", sandbox.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !gotSandbox.Platform.IsZero() {
		t.Errorf("sandbox platform = %q, want it left for its placement to give it", gotSandbox.Platform)
	}
}

// A database from before image kinds were recorded gains the columns, and
// every pool and image harness it already holds is OCI's — there was no other
// kind of image to run (ADR 26-10-09-106 §4). A manifest-file harness is no OCI
// image, and declared no driver, so it is left with no kind. A pool created afterwards has no
// kind until its agent declares one, and a later start does not claim OCI for
// it: a discovm pool runs no OCI image.
func TestMigrateBackfillsOCIImageKinds(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{Driver: gormdb.DriverSQLite, DSN: "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	project := model.Project{ID: "project-1", Name: "project"}
	if err := db.Write.Create(&project).Error; err != nil {
		t.Fatal(err)
	}
	provider := model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Name: "provider", Type: "docker"}
	if err := db.Write.Create(&provider).Error; err != nil {
		t.Fatal(err)
	}
	old := model.Pool{ID: "pool-old", ProjectID: project.ID, PoolManifest: model.PoolManifest{ProviderInstanceID: provider.ID, Name: "old"}}
	if err := db.Write.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	config := model.HarnessConfig{ID: "hc-1", ProjectID: project.ID, Slug: "shell", Name: "Shell"}
	if err := db.Write.Create(&config).Error; err != nil {
		t.Fatal(err)
	}
	manifestFile := model.HarnessConfig{ID: "hc-2", ProjectID: project.ID, Slug: "xcode", Name: "Xcode", Image: "file:///overlays/darwin-arm64/manifest.json"}
	if err := db.Write.Create(&manifestFile).Error; err != nil {
		t.Fatal(err)
	}
	// The schema those rows were written under: no image kind anywhere.
	for _, table := range []string{"pools", "harness_configs"} {
		if err := db.Write.Exec("ALTER TABLE " + table + " DROP COLUMN image_kind").Error; err != nil {
			t.Fatalf("drop %s.image_kind: %v", table, err)
		}
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	fresh := model.Pool{ID: "pool-new", ProjectID: project.ID, PoolManifest: model.PoolManifest{ProviderInstanceID: provider.ID, Name: "new"}}
	if err := db.Write.Create(&fresh).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	var gotConfig model.HarnessConfig
	if err := db.Write.First(&gotConfig, "id = ?", config.ID).Error; err != nil {
		t.Fatal(err)
	}
	if gotConfig.ImageKind != platform.OCI {
		t.Errorf("harness config image kind = %q, want oci", gotConfig.ImageKind)
	}
	var gotManifestFile model.HarnessConfig
	if err := db.Write.First(&gotManifestFile, "id = ?", manifestFile.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !gotManifestFile.ImageKind.IsZero() {
		t.Errorf("manifest-file harness image kind = %q, want none: a manifest file is no OCI image", gotManifestFile.ImageKind)
	}
	var gotOld, gotFresh model.Pool
	if err := db.Write.First(&gotOld, "id = ?", old.ID).Error; err != nil {
		t.Fatal(err)
	}
	if gotOld.ImageKind != platform.OCI {
		t.Errorf("pool from before kinds: image kind = %q, want oci", gotOld.ImageKind)
	}
	if err := db.Write.First(&gotFresh, "id = ?", fresh.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !gotFresh.ImageKind.IsZero() {
		t.Errorf("pool created since: image kind = %q, want it left for its agent to declare", gotFresh.ImageKind)
	}
}

// A harness config stored before bundles keeps everything it had, and gains a
// bundle and bundle digest that are empty until one is registered for it.
// No harness is an image (ADR 26-10-16-423 §10): a config from before bundles
// keeps everything its image's labels gave it but the image, and a config on a
// bundle loses the runnable twin it recorded beside it, and the platforms read
// from that twin. A manifest file is the one image a config keeps.
func TestMigrateRetiresHarnessImages(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{Driver: gormdb.DriverSQLite, DSN: "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	project := model.Project{ID: "project-1", Name: "project"}
	if err := db.Write.Create(&project).Error; err != nil {
		t.Fatal(err)
	}
	beforeBundles := model.HarnessConfig{
		ID: "hc-1", ProjectID: project.ID, Slug: "acme", Name: "Acme", Configured: true,
		Image: "example.com/acme:v1", ImageDigest: "sha256:acme", RunCommand: []string{"acme"},
		Env: map[string]string{"ACME": "1"}, Platforms: platform.NewSet(platform.Platform{OS: "linux", Arch: "amd64"}),
	}
	onBundle := model.HarnessConfig{
		ID: "hc-2", ProjectID: project.ID, Slug: "codex", Name: "Codex",
		Bundle: "example.com/codex:v1", BundleDigest: "sha256:bundle",
		Image: "example.com/codex-image:v1", ImageDigest: "sha256:twin", Platforms: platform.NewSet(platform.Platform{OS: "linux", Arch: "arm64"}),
	}
	manifestFile := model.HarnessConfig{
		ID: "hc-3", ProjectID: project.ID, Slug: "mac", Name: "Mac",
		Image: "file:///overlays/mac.json", ImageDigest: "sha256:file", Platforms: platform.NewSet(platform.Platform{OS: "darwin", Arch: "arm64"}),
	}
	for _, config := range []*model.HarnessConfig{&beforeBundles, &onBundle, &manifestFile} {
		if err := db.Write.Create(config).Error; err != nil {
			t.Fatal(err)
		}
	}
	// Twice: what one start clears, the next finds cleared.
	for range 2 {
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	read := func(id string) model.HarnessConfig {
		var got model.HarnessConfig
		if err := db.Write.First(&got, "id = ?", id).Error; err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := read(beforeBundles.ID); got.Image != "" || got.ImageDigest != "" || len(got.Platforms) != 0 ||
		!got.Configured || len(got.RunCommand) != 1 || got.Env["ACME"] != "1" || got.Bundle != "" {
		t.Errorf("config from before bundles = %+v, want its manifest kept and no image", got)
	}
	if got := read(onBundle.ID); got.Image != "" || got.ImageDigest != "" || len(got.Platforms) != 0 ||
		got.Bundle != onBundle.Bundle || got.BundleDigest != onBundle.BundleDigest {
		t.Errorf("config on a bundle = %+v, want its bundle and no twin", got)
	}
	if got := read(manifestFile.ID); got.Image != manifestFile.Image || got.ImageDigest != manifestFile.ImageDigest || len(got.Platforms) != 1 {
		t.Errorf("manifest-file config = %+v, want its file kept", got)
	}
}

func TestMigrateDeniesRequestsWhoseSandboxIsGone(t *testing.T) {
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

	for _, row := range []any{
		&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"},
		&model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "Docker"},
		&model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: "provider-1"}},
		&model.Sandbox{ID: "sb-live", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "sb-live"},
	} {
		if err := db.Write.Create(row).Error; err != nil {
			t.Fatalf("create %T: %v", row, err)
		}
	}
	for _, r := range []model.SecretRequest{
		{ID: "r-live", SandboxID: "sb-live"},
		{ID: "r-gone", SandboxID: "sb-gone"},
		{ID: "r-person", SandboxID: ""},
	} {
		r.ProjectID, r.RequestedBy, r.Status = "project-1", "user-1", model.SecretRequestStatusPending
		if err := db.Write.Create(&r).Error; err != nil {
			t.Fatalf("create request %s: %v", r.ID, err)
		}
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("upgrade migrate: %v", err)
	}
	var rows []model.SecretRequest
	if err := db.Write.Order("id").Find(&rows).Error; err != nil {
		t.Fatalf("list requests: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.ID] = r.Status
		if r.Status == model.SecretRequestStatusDenied && (r.DecidedAt == nil || r.DecidedBy != model.SecretBindingActorSystem) {
			t.Fatalf("request %s denied without being stamped decided", r.ID)
		}
	}
	want := map[string]string{
		"r-live": model.SecretRequestStatusPending, "r-gone": model.SecretRequestStatusDenied,
		"r-person": model.SecretRequestStatusPending,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests after migrate = %v, want %v", got, want)
	}
}

// A sandbox's pending egress push becomes a pending proxy policy push, once,
// however many times the server starts (ADR 26-10-16-470 §5).
func TestMigrateMovesEgressPushesToProxyPolicy(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{Driver: gormdb.DriverSQLite, DSN: "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, sql := range []string{
		"CREATE TABLE reconcile_dirty (resource_type text, resource_id text, PRIMARY KEY (resource_type, resource_id))",
		"INSERT INTO reconcile_dirty VALUES ('sandboxEgress', 'project-1/sb-1'), ('sandboxEgress', 'project-1/sb-2'), ('sandboxProxyPolicy', 'project-1/sb-2'), ('pool', 'project-1/pool-1')",
	} {
		if err := db.Write.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var rows []struct{ ResourceType, ResourceID string }
	if err := db.Write.Raw("SELECT resource_type, resource_id FROM reconcile_dirty ORDER BY resource_type, resource_id").Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	want := []struct{ ResourceType, ResourceID string }{
		{"pool", "project-1/pool-1"},
		{"sandboxProxyPolicy", "project-1/sb-1"},
		{"sandboxProxyPolicy", "project-1/sb-2"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v", rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("rows = %+v, want %+v", rows, want)
		}
	}
}

// A kind a release before ADR 26-10-15-629 stored as discovm/<driver> is
// respelled vm/<driver> on start, on pools and harnesses alike, and the rows
// read back; other kinds are left as they are, and a second start is a no-op.
func TestMigrateRespellsDiscoVMImageKinds(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{Driver: gormdb.DriverSQLite, DSN: "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	project := model.Project{ID: "project-1", Name: "project"}
	if err := db.Write.Create(&project).Error; err != nil {
		t.Fatal(err)
	}
	provider := model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Name: "provider", Type: "docker"}
	if err := db.Write.Create(&provider).Error; err != nil {
		t.Fatal(err)
	}
	for _, pool := range []model.Pool{
		{ID: "pool-vm", ProjectID: project.ID, PoolManifest: model.PoolManifest{ProviderInstanceID: provider.ID, Name: "vm"}},
		{ID: "pool-oci", ProjectID: project.ID, PoolManifest: model.PoolManifest{ProviderInstanceID: provider.ID, Name: "oci"}},
	} {
		if err := db.Write.Create(&pool).Error; err != nil {
			t.Fatal(err)
		}
	}
	config := model.HarnessConfig{ID: "hc-1", ProjectID: project.ID, Slug: "xcode", Name: "Xcode", Image: "file:///overlays/darwin-arm64/manifest.json"}
	if err := db.Write.Create(&config).Error; err != nil {
		t.Fatal(err)
	}
	// What a release before the respelling wrote, which no longer parses.
	for table, rows := range map[string]map[string]string{
		"pools":           {"pool-vm": "discovm/boxd", "pool-oci": "oci"},
		"harness_configs": {"hc-1": "discovm/vz"},
	} {
		for id, kind := range rows {
			if err := db.Write.Exec("UPDATE "+table+" SET image_kind = ? WHERE id = ?", kind, id).Error; err != nil {
				t.Fatalf("store %s.%s: %v", table, id, err)
			}
		}
	}

	for range 2 {
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}

	for id, want := range map[string]platform.ImageKind{"pool-vm": platform.VM("boxd"), "pool-oci": platform.OCI} {
		var got model.Pool
		if err := db.Write.First(&got, "id = ?", id).Error; err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if got.ImageKind != want {
			t.Errorf("%s image kind = %q, want %q", id, got.ImageKind, want)
		}
	}
	var gotConfig model.HarnessConfig
	if err := db.Write.First(&gotConfig, "id = ?", config.ID).Error; err != nil {
		t.Fatalf("read harness config: %v", err)
	}
	if gotConfig.ImageKind != platform.VM("vz") {
		t.Errorf("harness config image kind = %q, want vm/vz", gotConfig.ImageKind)
	}
}

// A database from before save-back keeps its config rows and gains an initial
// CAS revision; accepted capture receipts are a separate additive table.
func TestHarnessCaptureMigrationPreservesConfig(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Write.Exec(`CREATE TABLE harness_configs (id text PRIMARY KEY, project_id text NOT NULL, name text)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Write.Exec(`INSERT INTO harness_configs(id,project_id,name) VALUES ('hc-before','project-before','Before')`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Write.AutoMigrate(&model.User{}, &model.Project{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Write.Create(&model.Project{ID: "project-before", OwnerUserID: "user-before", Name: "Before"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var cfg model.HarnessConfig
	if err := db.Write.First(&cfg, "id = ?", "hc-before").Error; err != nil {
		t.Fatal(err)
	}
	if cfg.Revision != 1 || cfg.Name != "Before" {
		t.Fatalf("migration changed existing config: revision=%d name=%q", cfg.Revision, cfg.Name)
	}
	if !db.Write.Migrator().HasTable(&model.HarnessCaptureReceipt{}) {
		t.Fatal("missing receipt table")
	}
}

// A pool made before agent TLS was the default keeps the plain HTTP its
// sandboxes were made with; one made after gets TLS, and a later start
// touches neither (ADR 26-10-29-481 §3).
func TestMigrateKeepsExistingPoolsOnPlainAgentEndpoints(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{Driver: gormdb.DriverSQLite, DSN: "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	project := model.Project{ID: "project-1", Name: "project"}
	if err := db.Write.Create(&project).Error; err != nil {
		t.Fatal(err)
	}
	provider := model.SandboxProviderInstance{ID: "provider-1", ProjectID: project.ID, Name: "provider", Type: "docker"}
	if err := db.Write.Create(&provider).Error; err != nil {
		t.Fatal(err)
	}
	old := model.Pool{ID: "pool-old", ProjectID: project.ID, PoolManifest: model.PoolManifest{ProviderInstanceID: provider.ID, Name: "old"}}
	if err := db.Write.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	// The schema that row was written under: no setting.
	if err := db.Write.Exec("ALTER TABLE pools DROP COLUMN disable_agent_endpoint_tls").Error; err != nil {
		t.Fatalf("drop pools.disable_agent_endpoint_tls: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	fresh := model.Pool{ID: "pool-new", ProjectID: project.ID, PoolManifest: model.PoolManifest{ProviderInstanceID: provider.ID, Name: "new"}}
	if err := db.Write.Create(&fresh).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	var gotOld, gotFresh model.Pool
	if err := db.Write.First(&gotOld, "id = ?", old.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Write.First(&gotFresh, "id = ?", fresh.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !gotOld.DisableAgentEndpointTLS {
		t.Error("a pool from before the setting serves its agents over TLS; want it kept on plain HTTP")
	}
	if gotFresh.DisableAgentEndpointTLS {
		t.Error("a pool made since the setting is on plain HTTP; want TLS by default")
	}
}
