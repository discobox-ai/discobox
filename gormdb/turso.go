package gormdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	turso "turso.tech/database/tursogo"
)

const tursoLocalDriverName = "turso"

func openTurso(dsn string, cfg Config) (*Pools, error) {
	if dsn == "" {
		return nil, fmt.Errorf("turso DSN is required")
	}

	localDSN, remoteURL, err := parseTursoDSN(dsn)
	if err != nil {
		return nil, err
	}
	if cfg.TursoDatabaseURL != "" {
		remoteURL = cfg.TursoDatabaseURL
	}
	if cfg.TursoRemoteURL != "" {
		remoteURL = cfg.TursoRemoteURL
	}

	if remoteURL != "" {
		cfg.TursoDatabaseURL = remoteURL
		return openSyncedTurso(localDSN, cfg)
	}

	return openSQLiteWithDriver(localDSN, cfg, tursoLocalDriverName, DriverTurso, false)
}

func openSyncedTurso(dsn string, cfg Config) (*Pools, error) {
	path := cleanTursoLocalDSN(dsn)
	path = strings.TrimPrefix(path, "file:")
	if !isTursoMemoryDSN(path) {
		dirPath := path
		if beforeQuery, _, ok := strings.Cut(dirPath, "?"); ok {
			dirPath = beforeQuery
		}
		dir := filepath.Dir(dirPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create turso database directory %s: %w", dir, err)
		}
	}

	syncDB, err := turso.NewTursoSyncDb(context.Background(), turso.TursoSyncDbConfig{
		Path:      path,
		RemoteUrl: cfg.TursoDatabaseURL,
		AuthToken: cfg.TursoAuthToken,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open synced turso database: %w", err)
	}

	writeSQLDB, err := syncDB.Connect(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to connect synced turso write pool: %w", err)
	}
	writeSQLDB.SetMaxOpenConns(1)
	writeSQLDB.SetMaxIdleConns(1)

	gormCfg := &gorm.Config{}
	if cfg.Logger != nil {
		gormCfg.Logger = cfg.Logger
	}

	writeDB, err := gorm.Open(sqlite.Dialector{
		DriverName: tursoLocalDriverName,
		Conn:       writeSQLDB,
	}, gormCfg)
	if err != nil {
		_ = writeSQLDB.Close()
		return nil, fmt.Errorf("failed to open synced turso write pool: %w", err)
	}

	readDB := writeDB
	if cfg.ReadDSN != "" {
		readSQLDB, err := syncDB.Connect(context.Background())
		if err != nil {
			_ = closeDB(writeDB)
			return nil, fmt.Errorf("failed to connect synced turso read pool: %w", err)
		}
		readSQLDB.SetMaxOpenConns(1)
		readSQLDB.SetMaxIdleConns(1)
		if _, err := readSQLDB.ExecContext(context.Background(), "PRAGMA query_only = 1"); err != nil {
			_ = closeDB(writeDB)
			_ = readSQLDB.Close()
			return nil, fmt.Errorf("failed to configure synced turso read pool as query-only: %w", err)
		}
		readDB, err = gorm.Open(sqlite.Dialector{
			DriverName: tursoLocalDriverName,
			Conn:       readSQLDB,
		}, gormCfg)
		if err != nil {
			_ = closeDB(writeDB)
			_ = readSQLDB.Close()
			return nil, fmt.Errorf("failed to open synced turso read pool: %w", err)
		}
	}

	return &Pools{Write: writeDB, Read: readDB, Driver: DriverTurso, TursoSync: tursoSync{db: syncDB}}, nil
}

func cleanTursoLocalDSN(dsn string) string {
	dsn = strings.TrimPrefix(dsn, "turso://")
	dsn = strings.TrimPrefix(dsn, "turso:")
	return CleanDSN(dsn)
}

func parseTursoDSN(dsn string) (localDSN string, remoteURL string, err error) {
	localDSN = cleanTursoLocalDSN(dsn)
	base, rawQuery, found := strings.Cut(localDSN, "?")
	if !found {
		return localDSN, "", nil
	}

	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", "", fmt.Errorf("failed to parse turso DSN query: %w", err)
	}
	remoteURL = values.Get("remote_url")
	values.Del("remote_url")

	encoded := values.Encode()
	if encoded == "" {
		return base, remoteURL, nil
	}
	return base + "?" + encoded, remoteURL, nil
}

func prepareSyncedTursoPath(dsn string) (string, error) {
	path := cleanTursoLocalDSN(dsn)
	path = strings.TrimPrefix(path, "file:")
	if isTursoMemoryDSN(path) {
		return path, nil
	}

	dirPath := path
	if beforeQuery, _, ok := strings.Cut(dirPath, "?"); ok {
		dirPath = beforeQuery
	}
	dir := filepath.Dir(dirPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create turso database directory %s: %w", dir, err)
	}
	return path, nil
}

func isTursoMemoryDSN(path string) bool {
	return path == ":memory:" || strings.HasPrefix(path, ":memory:")
}

type tursoSync struct {
	db *turso.TursoSyncDb
}

func (s tursoSync) Pull(ctx context.Context) (bool, error) {
	return s.db.Pull(ctx)
}

func (s tursoSync) Push(ctx context.Context) error {
	return s.db.Push(ctx)
}

func (s tursoSync) Checkpoint(ctx context.Context) error {
	return s.db.Checkpoint(ctx)
}

func (s tursoSync) Stats(ctx context.Context) (TursoSyncStats, error) {
	stats, err := s.db.Stats(ctx)
	if err != nil {
		return TursoSyncStats{}, err
	}
	return TursoSyncStats{
		CdcOperations:        stats.CdcOperations,
		MainWalSize:          stats.MainWalSize,
		RevertWalSize:        stats.RevertWalSize,
		LastPullUnixTime:     stats.LastPullUnixTime,
		LastPushUnixTime:     stats.LastPushUnixTime,
		NetworkSentBytes:     stats.NetworkSentBytes,
		NetworkReceivedBytes: stats.NetworkReceivedBytes,
		Revision:             stats.Revision,
	}, nil
}
