// Package database opens and migrates application database connections.
package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/obot-platform/discobox/gormdb"
	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/server/internal/store"
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
	if err := rejectLegacyTenantSchema(write); err != nil {
		return err
	}
	if err := write.AutoMigrate(model.AllModels()...); err != nil {
		return err
	}
	return write.AutoMigrate(store.JobModels()...)
}

func rejectLegacyTenantSchema(db *gorm.DB) error {
	for _, m := range append(model.AllModels(), store.JobModels()...) {
		if !db.Migrator().HasTable(m) || !db.Migrator().HasColumn(m, "tenant_id") {
			continue
		}
		return fmt.Errorf("legacy tenant schema detected on table %q: tenant-era databases are not migrated in place; export with a build that understands the tenant-era schema, start this server with a fresh database, then recreate or import needed resources into the current schema", tableName(db, m))
	}
	return nil
}

func tableName(db *gorm.DB, m any) string {
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(m); err != nil || stmt.Schema == nil {
		return ""
	}
	return stmt.Schema.Table
}

// Close closes the underlying database pools.
func (db *DB) Close() error {
	if db == nil || db.pools == nil {
		return nil
	}
	return db.pools.Close()
}
