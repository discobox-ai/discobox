package gormdb_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/gormdb"
	"gorm.io/gorm"
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

func TestDefaultLoggerIgnoresRecordNotFound(t *testing.T) {
	type row struct {
		ID string `gorm:"primaryKey"`
	}

	output := captureStdout(t, func() {
		pools, err := gormdb.Open(gormdb.Config{DSN: ":memory:"})
		if err != nil {
			t.Fatalf("open pools: %v", err)
		}
		defer func() {
			if err := pools.Close(); err != nil {
				t.Fatalf("close pools: %v", err)
			}
		}()
		if err := pools.Write.AutoMigrate(&row{}); err != nil {
			t.Fatalf("auto migrate: %v", err)
		}
		err = pools.Write.First(&row{}, "id = ?", "missing").Error
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("query error = %v, want record not found", err)
		}
	})
	if strings.Contains(output, "record not found") {
		t.Fatalf("default logger output contains record not found: %q", output)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	os.Stdout = writer
	defer func() {
		os.Stdout = oldStdout
	}()

	fn()

	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	return string(output)
}
