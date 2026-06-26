// Package database opens and migrates application database connections.
package database

import (
	"context"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/obot-platform/discobox/gormdb"
	"github.com/obot-platform/discobox/server/internal/model"
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
	if err := write.AutoMigrate(model.AllModels()...); err != nil {
		return err
	}
	if err := dropAgentConfigDeletedAt(write); err != nil {
		return err
	}
	if err := write.AutoMigrate(store.JobModels()...); err != nil {
		return err
	}
	return store.BackfillJobProjectIDs(ctx, write)
}

func dropAgentConfigDeletedAt(db *gorm.DB) error {
	if !db.Migrator().HasColumn(&model.AgentConfig{}, "deleted_at") {
		return nil
	}
	return db.Migrator().DropColumn(&model.AgentConfig{}, "deleted_at")
}

// Close closes the underlying database pools.
func (db *DB) Close() error {
	if db == nil || db.pools == nil {
		return nil
	}
	return db.pools.Close()
}
