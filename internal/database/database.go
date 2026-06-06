// Package database opens and migrates application database connections.
package database

import (
	"context"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/obot-platform/disco2/gormdb"
	"github.com/obot-platform/disco2/internal/model"
	"github.com/obot-platform/disco2/jobqueue/gormstore"
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

// Migrate runs GORM AutoMigrate for application models.
func (db *DB) Migrate(ctx context.Context) error {
	if err := db.Write.WithContext(ctx).AutoMigrate(model.AllModels()...); err != nil {
		return err
	}
	return gormstore.New(db.Write, db.Read).Migrate(ctx)
}

// Close closes the underlying database pools.
func (db *DB) Close() error {
	if db == nil || db.pools == nil {
		return nil
	}
	return db.pools.Close()
}
