package gormdb_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/gormdb"
)

func TestOpenMemoryTursoUsesGORMSQLiteDialector(t *testing.T) {
	ctx := context.Background()
	pools, err := gormdb.Open(gormdb.Config{Driver: gormdb.DriverTurso, DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open pools: %v", err)
	}
	t.Cleanup(func() {
		if err := pools.Close(); err != nil {
			t.Fatalf("close pools: %v", err)
		}
	})

	if pools.Driver != gormdb.DriverTurso {
		t.Fatalf("driver = %q, want %q", pools.Driver, gormdb.DriverTurso)
	}
	if pools.Write == nil || pools.Read == nil {
		t.Fatal("expected write and read pools")
	}
	if pools.Write != pools.Read {
		t.Fatal("expected in-memory Turso to reuse write pool for reads")
	}

	if err := pools.Write.WithContext(ctx).Exec("CREATE TABLE ok_table (id text PRIMARY KEY)").Error; err != nil {
		t.Fatalf("write pool create table: %v", err)
	}
	if err := pools.Write.WithContext(ctx).Exec("INSERT INTO ok_table (id) VALUES (?)", "one").Error; err != nil {
		t.Fatalf("write pool insert: %v", err)
	}

	var id string
	if err := pools.Read.WithContext(ctx).Raw("SELECT id FROM ok_table").Scan(&id).Error; err != nil {
		t.Fatalf("read pool select: %v", err)
	}
	if id != "one" {
		t.Fatalf("id = %q, want one", id)
	}
}

func TestOpenDetectsTursoDSN(t *testing.T) {
	pools, err := gormdb.Open(gormdb.Config{DSN: "turso::memory:"})
	if err != nil {
		t.Fatalf("open pools: %v", err)
	}
	t.Cleanup(func() {
		if err := pools.Close(); err != nil {
			t.Fatalf("close pools: %v", err)
		}
	})

	if pools.Driver != gormdb.DriverTurso {
		t.Fatalf("driver = %q, want %q", pools.Driver, gormdb.DriverTurso)
	}
	if pools.Write == nil || pools.Read == nil {
		t.Fatal("expected write and read pools")
	}
}

func TestOpenFileBackedTursoUsesSplitPools(t *testing.T) {
	ctx := context.Background()
	pools, err := gormdb.Open(gormdb.Config{Driver: gormdb.DriverTurso, DSN: filepath.Join(t.TempDir(), "test.db")})
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
		t.Fatal("expected separate pools for file-backed Turso")
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
	if got := readSQLDB.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("read max open conns = %d, want 1", got)
	}

	if err := pools.Write.WithContext(ctx).Exec("CREATE TABLE ok_table (id text)").Error; err != nil {
		t.Fatalf("write pool create table: %v", err)
	}
	if err := pools.Read.WithContext(ctx).Exec("CREATE TABLE should_fail (id text)").Error; err == nil {
		t.Fatal("expected read pool write to fail")
	}
}
