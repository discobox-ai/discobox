package gormdb_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/gormdb"
)

func TestDetectDriver(t *testing.T) {
	tests := map[string]gormdb.Driver{
		"postgres://user:pass@localhost/db":   gormdb.DriverPostgres,
		"postgresql://user:pass@localhost/db": gormdb.DriverPostgres,
		"turso://app.db":                      gormdb.DriverTurso,
		"turso:/tmp/app.db":                   gormdb.DriverTurso,
		"sqlite://app.db":                     gormdb.DriverSQLite,
		"sqlite3://app.db":                    gormdb.DriverSQLite,
		"file:app.db":                         gormdb.DriverSQLite,
		"app.db":                              gormdb.DriverSQLite,
		"app.sqlite":                          gormdb.DriverSQLite,
		":memory:":                            gormdb.DriverSQLite,
	}

	for dsn, want := range tests {
		if got := gormdb.DetectDriver(dsn); got != want {
			t.Fatalf("DetectDriver(%q) = %q, want %q", dsn, got, want)
		}
	}
}

func TestOpenRejectsUnsupportedDriver(t *testing.T) {
	_, err := gormdb.Open(gormdb.Config{DSN: "mysql://example"})
	if err == nil {
		t.Fatal("expected unsupported driver error")
	}
	if !strings.Contains(err.Error(), "unsupported database driver") {
		t.Fatalf("error = %v, want unsupported driver", err)
	}
}

func TestOpenFileBackedSQLiteUsesSplitPools(t *testing.T) {
	ctx := context.Background()
	pools, err := gormdb.Open(gormdb.Config{DSN: "sqlite3://" + filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("open pools: %v", err)
	}
	t.Cleanup(func() {
		if err := pools.Close(); err != nil {
			t.Fatalf("close pools: %v", err)
		}
	})

	if pools.Write == nil || pools.Read == nil {
		t.Fatal("expected write and read pools")
	}
	if pools.Write == pools.Read {
		t.Fatal("expected separate pools for file-backed SQLite")
	}

	writeSQLDB, err := pools.Write.DB()
	if err != nil {
		t.Fatalf("write sql db: %v", err)
	}
	if got := writeSQLDB.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("write max open conns = %d, want 1", got)
	}

	readSQLDB, err := pools.Read.DB()
	if err != nil {
		t.Fatalf("read sql db: %v", err)
	}
	if got := readSQLDB.Stats().MaxOpenConnections; got != 25 {
		t.Fatalf("read max open conns = %d, want 25", got)
	}

	if err := pools.Write.WithContext(ctx).Exec("CREATE TABLE ok_table (id text)").Error; err != nil {
		t.Fatalf("write pool create table: %v", err)
	}
	if err := pools.Read.WithContext(ctx).Exec("CREATE TABLE should_fail (id text)").Error; err == nil {
		t.Fatal("expected read pool write to fail")
	}
}

func TestOpenMemorySQLiteReusesWritePoolForReads(t *testing.T) {
	pools, err := gormdb.Open(gormdb.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open pools: %v", err)
	}
	t.Cleanup(func() {
		if err := pools.Close(); err != nil {
			t.Fatalf("close pools: %v", err)
		}
	})

	if pools.Write == nil || pools.Read == nil {
		t.Fatal("expected write and read pools")
	}
	if pools.Write != pools.Read {
		t.Fatal("expected in-memory SQLite to reuse write pool for reads")
	}
}
