package database_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/obot-platform/discobox/gormdb"
	"github.com/obot-platform/discobox/server/internal/database"
	"gorm.io/gorm"
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

func TestMigrateDropsAgentConfigDeletedAt(t *testing.T) {
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

	if err := db.Write.AutoMigrate(&legacyAgentConfig{}); err != nil {
		t.Fatalf("create legacy agent config table: %v", err)
	}
	if !db.Write.Migrator().HasColumn(&legacyAgentConfig{}, "deleted_at") {
		t.Fatalf("legacy schema missing deleted_at before migration")
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	if db.Write.Migrator().HasColumn(&legacyAgentConfig{}, "deleted_at") {
		t.Fatalf("agent_configs still has deleted_at after migration")
	}
}

type legacyAgentConfig struct {
	ID             string         `gorm:"primaryKey;type:text"`
	ProjectID      string         `gorm:"column:project_id;not null;type:text;index;uniqueIndex:idx_agent_config_project_name,priority:1"`
	Name           string         `gorm:"column:name;not null;type:text;uniqueIndex:idx_agent_config_project_name,priority:2"`
	InstallCommand string         `gorm:"column:install_command;type:text"`
	RunCommand     string         `gorm:"column:run_command;not null;type:text"`
	CreatedAt      time.Time      `gorm:"autoCreateTime"`
	UpdatedAt      time.Time      `gorm:"autoUpdateTime"`
	DeletedAt      gorm.DeletedAt `gorm:"index"`
}

func (legacyAgentConfig) TableName() string { return "agent_configs" }

func fileExists(path string) bool {
	matches, err := filepath.Glob(path)
	return err == nil && len(matches) == 1
}
