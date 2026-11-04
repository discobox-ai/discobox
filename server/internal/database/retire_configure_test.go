package database_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/x/gormdb"
)

// Upgrading from configure (ADR 26-11-02-152) marks each unfinished configure
// sandbox for deletion with its dirty mark, leaves every run sandbox alone,
// drops the harness mode and the columns only configure used, and keeps the
// saved overlay save-back writes. A second run changes nothing.
func TestMigrateRetiresTheConfigureFlow(t *testing.T) {
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
	if _, err := reconcile.New(db.Write, reconcile.Options{}); err != nil {
		t.Fatalf("reconcile table: %v", err)
	}
	write := db.Write.WithContext(ctx)
	// What a database written while configure existed carried.
	for _, stmt := range []string{
		`ALTER TABLE sandboxes ADD COLUMN harness_mode text NOT NULL DEFAULT 'run'`,
		`ALTER TABLE harness_configs ADD COLUMN configured numeric NOT NULL DEFAULT false`,
		`ALTER TABLE harness_configs ADD COLUMN configure_error text DEFAULT ''`,
		`ALTER TABLE harness_configs ADD COLUMN configure_sandbox_id text DEFAULT ''`,
		`ALTER TABLE harness_configs ADD COLUMN configure_revision integer NOT NULL DEFAULT 0`,
		`ALTER TABLE harness_configs ADD COLUMN configure_secret_revisions text`,
		`ALTER TABLE harness_configs ADD COLUMN config_command text`,
		`ALTER TABLE harness_configs ADD COLUMN config_reminder text NOT NULL DEFAULT ''`,
		`ALTER TABLE harness_configs ADD COLUMN config_ports text`,
	} {
		if err := write.Exec(stmt).Error; err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := write.Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := write.Create(&model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "Docker"}).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if err := write.Create(&model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: "provider-1"}}).Error; err != nil {
		t.Fatalf("create pool: %v", err)
	}
	for _, id := range []string{"sbx-configure", "sbx-run"} {
		if err := write.Create(&model.Sandbox{ID: id, ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: id}).Error; err != nil {
			t.Fatalf("create sandbox %s: %v", id, err)
		}
	}
	for _, stmt := range []string{
		`UPDATE sandboxes SET harness_mode = 'config', generation = 3, observed_generation = 3 WHERE id = 'sbx-configure'`,
		`UPDATE sandboxes SET generation = 2, observed_generation = 2 WHERE id = 'sbx-run'`,
		`INSERT INTO harness_configs (id, project_id, slug, name, configured, configure_sandbox_id, config_command, configured_files, configured_secret_ids)
			VALUES ('hc-1', 'project-1', 'codex', 'Codex', true, 'sbx-configure', '["configure-codex"]',
			'[{"path":".codex/config.toml","content":"model = \"saved\"\n"}]', '["sec-saved"]')`,
	} {
		if err := write.Exec(stmt).Error; err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	for run := 1; run <= 2; run++ {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("run %d: migrate: %v", run, err)
		}
		var configure, kept model.Sandbox
		if err := write.First(&configure, "id = ?", "sbx-configure").Error; err != nil {
			t.Fatalf("run %d: read configure sandbox: %v", run, err)
		}
		if configure.DesiredState != model.DesiredStateDeleted || configure.Generation != 4 {
			t.Fatalf("run %d: configure sandbox = desired %q, generation %d; want marked for deletion once", run, configure.DesiredState, configure.Generation)
		}
		if err := write.First(&kept, "id = ?", "sbx-run").Error; err != nil {
			t.Fatalf("run %d: read run sandbox: %v", run, err)
		}
		if kept.DesiredState == model.DesiredStateDeleted || kept.Generation != 2 {
			t.Fatalf("run %d: run sandbox = desired %q, generation %d; want it untouched", run, kept.DesiredState, kept.Generation)
		}
		var marks int64
		if err := write.Table("reconcile_dirty").Where("resource_type = ? AND resource_id = ?", "sandbox", "project-1/sbx-configure").Count(&marks).Error; err != nil {
			t.Fatal(err)
		}
		if marks != 1 {
			t.Fatalf("run %d: dirty marks for the configure sandbox = %d, want 1", run, marks)
		}

		migrator := db.Write.Migrator()
		if migrator.HasColumn("sandboxes", "harness_mode") {
			t.Fatalf("run %d: sandboxes.harness_mode survived", run)
		}
		for _, column := range []string{"configured", "configure_error", "configure_sandbox_id", "configure_revision", "configure_secret_revisions", "config_command", "config_reminder", "config_ports"} {
			if migrator.HasColumn("harness_configs", column) {
				t.Fatalf("run %d: harness_configs.%s survived", run, column)
			}
		}
		var config model.HarnessConfig
		if err := write.First(&config, "id = ?", "hc-1").Error; err != nil {
			t.Fatalf("run %d: read harness config: %v", run, err)
		}
		if len(config.ConfiguredFiles) != 1 || len(config.ConfiguredSecretIDs) != 1 || config.ConfiguredSecretIDs[0] != "sec-saved" {
			t.Fatalf("run %d: saved overlay = files %v, secrets %v; want it kept", run, config.ConfiguredFiles, config.ConfiguredSecretIDs)
		}
	}
}
