// Package gormdb opens GORM connection pools using Discobot's database
// configuration patterns.
package gormdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Driver identifies the database backend.
type Driver string

const (
	DriverSQLite   Driver = "sqlite"
	DriverPostgres Driver = "postgres"
	DriverTurso    Driver = "turso"
)

// Config controls Open.
type Config struct {
	// Driver is used by Open. If empty, Open detects it from DSN.
	Driver Driver

	// DSN is used by Open. SQLite accepts raw paths, file:, sqlite://,
	// sqlite3://, and :memory:. Postgres accepts postgres:// and postgresql://.
	// Turso support requires building with -tags turso and accepts turso: and
	// turso:// DSNs. Turso DSNs use the path as the local database path and may
	// include remote_url to sync that local database with Turso Cloud. Set
	// TursoAuthToken separately for remote authentication.
	DSN string

	// ReadDSN optionally opens a separate read pool for Postgres and Turso
	// Cloud sync. SQLite and local Turso ignore ReadDSN because their read pools
	// are derived from DSN.
	ReadDSN string

	// TursoDatabaseURL enables Turso Cloud sync when Driver is DriverTurso. It
	// overrides any remote_url query parameter in DSN.
	TursoDatabaseURL string

	// TursoRemoteURL is an alias for TursoDatabaseURL.
	TursoRemoteURL string

	// TursoAuthToken is the bearer token used with TursoDatabaseURL.
	TursoAuthToken string

	// Logger is passed to both GORM pools. When nil, GORM's default logger is
	// used.
	Logger logger.Interface
}

// Pools contains the write and read GORM pools.
type Pools struct {
	Write  *gorm.DB
	Read   *gorm.DB
	Driver Driver

	// TursoSync is set for synced Turso databases. It is nil for SQLite,
	// Postgres, and local-only Turso.
	TursoSync TursoSync
}

// TursoSync exposes explicit sync operations for a Turso Cloud synced local
// database.
type TursoSync interface {
	Pull(context.Context) (bool, error)
	Push(context.Context) error
	Checkpoint(context.Context) error
	Stats(context.Context) (TursoSyncStats, error)
}

// TursoSyncStats contains Turso Cloud sync statistics.
type TursoSyncStats struct {
	CdcOperations        int64
	MainWalSize          int64
	RevertWalSize        int64
	LastPullUnixTime     int64
	LastPushUnixTime     int64
	NetworkSentBytes     int64
	NetworkReceivedBytes int64
	Revision             string
}

// Open detects the configured driver and opens GORM pools.
func Open(cfg Config) (*Pools, error) {
	driver := cfg.Driver
	if driver == "" {
		driver = DetectDriver(cfg.DSN)
	}

	switch driver {
	case DriverSQLite:
		return openSQLite(cfg.DSN, cfg)
	case DriverPostgres:
		return openPostgres(cfg.DSN, cfg)
	case DriverTurso:
		return openTurso(cfg.DSN, cfg)
	default:
		return nil, fmt.Errorf("unsupported database driver: %s", driver)
	}
}

// DetectDriver detects a database driver from a DSN.
func DetectDriver(dsn string) Driver {
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		return DriverPostgres
	case strings.HasPrefix(dsn, "turso://"), strings.HasPrefix(dsn, "turso:"):
		return DriverTurso
	case strings.HasPrefix(dsn, "sqlite://"), strings.HasPrefix(dsn, "sqlite3://"), strings.HasPrefix(dsn, "file:"):
		return DriverSQLite
	case strings.HasSuffix(dsn, ".db"), strings.HasSuffix(dsn, ".sqlite"), dsn == ":memory:", strings.HasPrefix(dsn, ":memory:"):
		return DriverSQLite
	default:
		return ""
	}
}

// CleanDSN strips driver prefixes that are not accepted by the underlying
// GORM driver.
func CleanDSN(dsn string) string {
	dsn = strings.TrimPrefix(dsn, "sqlite3://")
	dsn = strings.TrimPrefix(dsn, "sqlite://")
	return dsn
}

// openSQLite opens SQLite read/write pools.
//
// Accepted DSN forms are raw paths, file: paths, sqlite:// paths, sqlite3://
// paths, and :memory:.
//
// File-backed SQLite databases use separate pools: Write has one connection and
// Read is read-only with multiple connections. In-memory databases reuse Write
// for reads because separate in-memory SQLite connections do not share state.
func openSQLite(dsn string, cfg Config) (*Pools, error) {
	return openSQLiteWithDriver(dsn, cfg, sqlite.DriverName, DriverSQLite, true)
}

func openSQLiteWithDriver(dsn string, cfg Config, driverName string, driver Driver, useFileURI bool) (*Pools, error) {
	rawPath := CleanDSN(dsn)
	rawPath = strings.TrimPrefix(rawPath, "file:")
	isMemory := rawPath == ":memory:" || strings.HasPrefix(rawPath, ":memory:")

	if !isMemory {
		dir := filepath.Dir(rawPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create database directory %s: %w", dir, err)
		}
	}

	baseDSN := rawPath
	if !isMemory && useFileURI {
		baseDSN = "file:" + rawPath
	}

	basePragmas := []string{
		"_pragma=journal_mode(WAL)",
		"_pragma=busy_timeout(5000)",
		"_pragma=foreign_keys(1)",
		"_pragma=synchronous(NORMAL)",
	}

	gormCfg := &gorm.Config{}
	if cfg.Logger != nil {
		gormCfg.Logger = cfg.Logger
	}

	writeParams := append(basePragmas, "_txlock=immediate")
	if !isMemory {
		writeParams = append(writeParams, "mode=rwc")
	}
	writeDB, err := gorm.Open(sqlite.Dialector{
		DriverName: driverName,
		DSN:        appendParams(baseDSN, writeParams),
	}, gormCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s write pool: %w", driver, err)
	}
	writeSQLDB, err := writeDB.DB()
	if err != nil {
		return nil, fmt.Errorf("failed to get write pool sql.DB: %w", err)
	}
	writeSQLDB.SetMaxOpenConns(1)
	writeSQLDB.SetMaxIdleConns(1)

	if isMemory {
		return &Pools{Write: writeDB, Read: writeDB, Driver: driver}, nil
	}

	readParams := append(basePragmas, "mode=ro", "_pragma=query_only(1)")
	readDB, err := gorm.Open(sqlite.Dialector{
		DriverName: driverName,
		DSN:        appendParams(baseDSN, readParams),
	}, gormCfg)
	if err != nil {
		_ = closeDB(writeDB)
		return nil, fmt.Errorf("failed to open %s read pool: %w", driver, err)
	}
	readSQLDB, err := readDB.DB()
	if err != nil {
		_ = closeDB(writeDB)
		return nil, fmt.Errorf("failed to get read pool sql.DB: %w", err)
	}
	if driver == DriverTurso {
		readSQLDB.SetMaxOpenConns(1)
		readSQLDB.SetMaxIdleConns(1)
		if err := readDB.Exec("PRAGMA query_only = 1").Error; err != nil {
			_ = closeDB(writeDB)
			_ = closeDB(readDB)
			return nil, fmt.Errorf("failed to configure turso read pool as query-only: %w", err)
		}
	} else {
		readSQLDB.SetMaxOpenConns(25)
		readSQLDB.SetMaxIdleConns(4)
	}

	return &Pools{Write: writeDB, Read: readDB, Driver: driver}, nil
}

// openPostgres opens Postgres GORM pools.
//
// By default Write and Read point to the same pool. If cfg.ReadDSN is set, Read
// uses a separate pool opened from that DSN.
func openPostgres(dsn string, cfg Config) (*Pools, error) {
	if dsn == "" {
		return nil, fmt.Errorf("postgres DSN is required")
	}

	gormCfg := &gorm.Config{}
	if cfg.Logger != nil {
		gormCfg.Logger = cfg.Logger
	}

	writeDB, err := gorm.Open(postgres.Open(dsn), gormCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to open postgres write pool: %w", err)
	}
	if err := configureSQLPool(writeDB, 25, 5); err != nil {
		_ = closeDB(writeDB)
		return nil, fmt.Errorf("failed to configure postgres write pool: %w", err)
	}

	readDB := writeDB
	if cfg.ReadDSN != "" {
		readDB, err = gorm.Open(postgres.Open(cfg.ReadDSN), gormCfg)
		if err != nil {
			_ = closeDB(writeDB)
			return nil, fmt.Errorf("failed to open postgres read pool: %w", err)
		}
		if err := configureSQLPool(readDB, 25, 5); err != nil {
			_ = closeDB(writeDB)
			_ = closeDB(readDB)
			return nil, fmt.Errorf("failed to configure postgres read pool: %w", err)
		}
	}

	return &Pools{Write: writeDB, Read: readDB, Driver: DriverPostgres}, nil
}

// Close closes both pools.
func (p *Pools) Close() error {
	if p == nil {
		return nil
	}
	if err := closeDB(p.Write); err != nil {
		return err
	}
	if p.Read != nil && p.Read != p.Write {
		return closeDB(p.Read)
	}
	return nil
}

func appendParams(base string, params []string) string {
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + strings.Join(params, "&")
}

func closeDB(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func configureSQLPool(db *gorm.DB, maxOpenConns, maxIdleConns int) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxIdleConns)
	return nil
}
