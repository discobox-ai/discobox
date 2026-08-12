// Package database opens and migrates application database connections.
package database

import (
	"context"
	"fmt"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/obot-platform/discobox/gormdb"
	"github.com/obot-platform/discobox/server/internal/model"
)

// DB wraps the application write/read GORM pools.
type DB struct {
	Write *gorm.DB
	Read  *gorm.DB

	pools *gormdb.Pools
}

// Config controls New.
type Config struct {
	Driver  gormdb.Driver
	DSN     string
	ReadDSN string
	Logger  logger.Interface
}

// New opens database pools using the shared GORM database opener.
func New(cfg Config) (*DB, error) {
	pools, err := gormdb.Open(gormdb.Config{
		Driver:  cfg.Driver,
		DSN:     cfg.DSN,
		ReadDSN: cfg.ReadDSN,
		Logger:  cfg.Logger,
	})
	if err != nil {
		return nil, err
	}
	return &DB{
		Write: pools.Write,
		Read:  pools.Read,
		pools: pools,
	}, nil
}

// Migrate runs all current application migrations.
//
// It intentionally does not perform tenant-era compatibility rewrites. Databases
// with legacy tenant_id columns or tenant-scoped keys must be exported or
// recreated into the current single-database schema before running this server.
func (db *DB) Migrate(ctx context.Context) error {
	write := db.Write.WithContext(ctx)
	if err := write.AutoMigrate(model.AllModels()...); err != nil {
		return err
	}
	if err := migrateLibkrunProviderType(write); err != nil {
		return err
	}
	if err := dropLegacyPoolBootstrapTokenConstraint(write); err != nil {
		return err
	}
	if err := dropJobQueueArtifacts(write); err != nil {
		return err
	}
	if err := dropLegacyProjectSlug(write); err != nil {
		return err
	}
	return dropSandboxResourceRequestColumns(write)
}

const (
	legacyLibkrunProviderType = "local-vm"
	libkrunProviderType       = "libkrun"

	legacyPoolBootForeignKey  = "fk_pools_bootstrap_tokens"
	poolBootCascadeForeignKey = "fk_pool_bootstrap_tokens_pool"
)

// migrateLibkrunProviderType preserves provider and pool identities while
// replacing the pre-release local-vm backend name with its implementation name.
func migrateLibkrunProviderType(db *gorm.DB) error {
	return db.Model(&model.SandboxProviderInstance{}).
		Where("type = ?", legacyLibkrunProviderType).
		Update("type", libkrunProviderType).Error
}

// dropLegacyPoolBootstrapTokenConstraint completes the upgrade from the
// original restrictive pool-token foreign key. AutoMigrate creates the newly
// named cascading constraint first; only then is it safe to remove the old
// constraint. Using GORM's migrator keeps the same migration valid for SQLite
// and PostgreSQL.
func dropLegacyPoolBootstrapTokenConstraint(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasConstraint("pool_bootstrap_tokens", legacyPoolBootForeignKey) {
		return nil
	}
	if !migrator.HasConstraint("pool_bootstrap_tokens", poolBootCascadeForeignKey) {
		return fmt.Errorf("pool bootstrap token cascade constraint %q is missing", poolBootCascadeForeignKey)
	}
	return migrator.DropConstraint("pool_bootstrap_tokens", legacyPoolBootForeignKey)
}

// dropJobQueueArtifacts removes the retired job-queue schema: the job and
// leader tables, and the last_job_id lifecycle columns. Reconciliation now
// rides the reconcile engine's dirty set.
func dropJobQueueArtifacts(db *gorm.DB) error {
	drop := func(tx *gorm.DB) error {
		for _, table := range []string{"jobqueue_jobs", "jobqueue_leaders"} {
			if tx.Migrator().HasTable(table) {
				if err := tx.Migrator().DropTable(table); err != nil {
					return err
				}
			}
		}
		for _, m := range []any{&model.Sandbox{}} {
			if tx.Migrator().HasColumn(m, "last_job_id") {
				if err := tx.Migrator().DropColumn(m, "last_job_id"); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if db.Name() != "sqlite" {
		return drop(db)
	}
	// SQLite drops columns by rebuilding the table (create new, copy, DROP TABLE,
	// rename). With foreign_keys ON, dropping `workers` fails because sandboxes
	// and bootstrap tokens reference it. Follow SQLite's documented ALTER TABLE
	// procedure: disable the pragma around the rebuild. Pragmas are
	// per-connection, so pin one connection for the whole operation.
	return db.Connection(func(tx *gorm.DB) error {
		if err := tx.Exec("PRAGMA foreign_keys = OFF").Error; err != nil {
			return err
		}
		defer tx.Exec("PRAGMA foreign_keys = ON")
		return drop(tx)
	})
}

// dropLegacyProjectSlug removes the retired projects.slug column (ADR 0023 §5).
// AutoMigrate adds columns but never drops them, so a database created before
// the column was retired keeps it — declared NOT NULL with no default, which
// fails every project write the moment the model stops populating it.
//
// The rebuild is the same SQLite dance dropJobQueueArtifacts documents: the
// projects table is referenced by most of the schema, so the foreign-key pragma
// comes off around it.
func dropLegacyProjectSlug(db *gorm.DB) error {
	return dropRetiredColumn(db, &model.Project{}, "projects", "slug")
}

// dropSandboxResourceRequestColumns removes the retired per-sandbox
// cpu_vcpus/memory_bytes/storage_bytes request columns (docs/adr/0029):
// sandboxes no longer reserve a slice of their pool's envelope, so nothing
// writes these columns anymore. The same-named columns on pools are the
// envelope itself and are untouched.
func dropSandboxResourceRequestColumns(db *gorm.DB) error {
	for _, column := range []string{"cpu_vcpus", "memory_bytes", "storage_bytes"} {
		if err := dropRetiredColumn(db, &model.Sandbox{}, "sandboxes", column); err != nil {
			return err
		}
	}
	return nil
}

// dropRetiredColumn removes a column that is no longer part of a model.
//
// It issues the DDL directly rather than through Migrator().DropColumn. On
// SQLite that helper rebuilds the table by rewriting its stored CREATE TABLE
// text, and it only matches the column when the DDL quotes the identifier the
// way GORM writes it. A column added by anything else is left in place and the
// call still reports success. `ALTER TABLE ... DROP COLUMN` has no such
// dependency and is supported by both SQLite (3.35+) and PostgreSQL.
//
// The foreign-key pragma comes off around the statement for the same reason
// dropJobQueueArtifacts documents: SQLite implements the drop by rebuilding the
// table, which trips references from other tables. Pragmas are per-connection,
// so the rebuild is pinned to one connection.
func dropRetiredColumn(db *gorm.DB, model any, table, column string) error {
	drop := func(tx *gorm.DB) error {
		if !tx.Migrator().HasColumn(model, column) {
			return nil
		}
		if tx.Name() == "sqlite" {
			// SQLite's ALTER TABLE DROP COLUMN refuses a column that is still
			// covered by an index: a database created before the column was
			// retired can carry one (e.g. a uniqueIndex struct tag from when
			// the field still existed), and AutoMigrate never drops indexes
			// any more than it drops columns. Postgres has no such
			// restriction — dropping a column there drops its indexes too.
			if err := dropIndexesCoveringColumn(tx, table, column); err != nil {
				return err
			}
		}
		return tx.Exec(fmt.Sprintf("ALTER TABLE %q DROP COLUMN %q", table, column)).Error
	}
	if db.Name() != "sqlite" {
		return drop(db)
	}
	return db.Connection(func(tx *gorm.DB) error {
		if err := tx.Exec("PRAGMA foreign_keys = OFF").Error; err != nil {
			return err
		}
		defer tx.Exec("PRAGMA foreign_keys = ON")
		return drop(tx)
	})
}

// dropIndexesCoveringColumn drops every SQLite index that references column,
// so a subsequent ALTER TABLE DROP COLUMN is not refused. Indexes whose name
// starts with sqlite_ back an inline PRIMARY KEY/UNIQUE constraint rather than
// a real CREATE INDEX statement; SQLite does not allow dropping those
// directly, and a retired application column is never part of one.
func dropIndexesCoveringColumn(tx *gorm.DB, table, column string) error {
	var indexes []struct {
		Name string `gorm:"column:name"`
	}
	if err := tx.Raw(fmt.Sprintf("PRAGMA index_list(%q)", table)).Scan(&indexes).Error; err != nil {
		return err
	}
	for _, index := range indexes {
		if strings.HasPrefix(index.Name, "sqlite_") {
			continue
		}
		var columns []struct {
			Name string `gorm:"column:name"`
		}
		if err := tx.Raw(fmt.Sprintf("PRAGMA index_info(%q)", index.Name)).Scan(&columns).Error; err != nil {
			return err
		}
		for _, c := range columns {
			if c.Name != column {
				continue
			}
			if err := tx.Exec(fmt.Sprintf("DROP INDEX %q", index.Name)).Error; err != nil {
				return err
			}
			break
		}
	}
	return nil
}

// Close closes the underlying database pools.
func (db *DB) Close() error {
	if db == nil || db.pools == nil {
		return nil
	}
	return db.pools.Close()
}
