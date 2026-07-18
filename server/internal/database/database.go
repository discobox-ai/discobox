// Package database opens and migrates application database connections.
package database

import (
	"context"

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
	return dropJobQueueArtifacts(write)
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

// Close closes the underlying database pools.
func (db *DB) Close() error {
	if db == nil || db.pools == nil {
		return nil
	}
	return db.pools.Close()
}
