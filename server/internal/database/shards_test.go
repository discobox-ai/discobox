package database_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/obot-platform/discobox/gormdb"
	"github.com/obot-platform/discobox/server/internal/database"
)

func TestTenantSQLiteDSN(t *testing.T) {
	tests := []struct {
		name     string
		baseDSN  string
		tenantID string
		want     string
	}{
		{
			name:     "sqlite3 prefix",
			baseDSN:  "sqlite3:///data/discobox.db",
			tenantID: "tenant-1",
			want:     "sqlite3:///data/discobox.tenant-1.db",
		},
		{
			name:     "sqlite prefix",
			baseDSN:  "sqlite:///data/discobox.sqlite",
			tenantID: "tenant-1",
			want:     "sqlite:///data/discobox.tenant-1.sqlite",
		},
		{
			name:     "file prefix",
			baseDSN:  "file:/data/discobox.db",
			tenantID: "tenant-1",
			want:     "file:/data/discobox.tenant-1.db",
		},
		{
			name:     "raw path",
			baseDSN:  "/data/discobox.db",
			tenantID: "tenant-1",
			want:     "/data/discobox.tenant-1.db",
		},
		{
			name:     "memory unchanged",
			baseDSN:  ":memory:",
			tenantID: "tenant-1",
			want:     ":memory:",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := database.TenantSQLiteDSN(tt.baseDSN, tt.tenantID); got != tt.want {
				t.Fatalf("TenantSQLiteDSN() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolverCreatesSQLiteDatabasePerTenant(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	baseDSN := "sqlite3://" + filepath.Join(dir, "discobox.db")
	resolver := database.NewResolver(database.ResolverConfig{
		Config: database.Config{
			Driver: gormdb.DriverSQLite,
			DSN:    baseDSN,
		},
		MigrateOnOpen: true,
	})
	t.Cleanup(func() {
		if err := resolver.Close(); err != nil {
			t.Fatalf("close resolver: %v", err)
		}
	})

	db1, err := resolver.Resolve(ctx, "tenant-1")
	if err != nil {
		t.Fatalf("resolve tenant-1: %v", err)
	}
	db2, err := resolver.Resolve(ctx, "tenant-2")
	if err != nil {
		t.Fatalf("resolve tenant-2: %v", err)
	}
	if db1 == db2 {
		t.Fatalf("different tenants resolved to the same sqlite DB")
	}
	if _, err := resolver.Resolve(ctx, "../bad"); err == nil {
		t.Fatalf("resolve unsafe tenant ID succeeded")
	}
	for _, file := range []string{"discobox.tenant-1.db", "discobox.tenant-2.db"} {
		if !fileExists(filepath.Join(dir, file)) {
			t.Fatalf("expected tenant database %s to exist", file)
		}
	}
}

func TestGlobalAndTenantSQLiteSchemasAreSeparated(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	baseDSN := "sqlite3://" + filepath.Join(dir, "discobox.db")
	resolver := database.NewResolver(database.ResolverConfig{
		Config: database.Config{
			Driver: gormdb.DriverSQLite,
			DSN:    baseDSN,
		},
		MigrateOnOpen: true,
	})
	t.Cleanup(func() {
		if err := resolver.Close(); err != nil {
			t.Fatalf("close resolver: %v", err)
		}
	})

	globalDB, err := resolver.ResolveGlobal(ctx)
	if err != nil {
		t.Fatalf("resolve global: %v", err)
	}
	tenantDB, err := resolver.Resolve(ctx, "tenant-1")
	if err != nil {
		t.Fatalf("resolve tenant: %v", err)
	}

	for _, table := range []string{"tenants", "users"} {
		if !globalDB.Write.Migrator().HasTable(table) {
			t.Fatalf("global schema missing table %s", table)
		}
	}
	for _, table := range []string{"projects", "sandboxes", "sandbox_provider_instances", "workers", "project_events", "jobqueue_jobs"} {
		if globalDB.Write.Migrator().HasTable(table) {
			t.Fatalf("global schema unexpectedly has tenant table %s", table)
		}
	}

	for _, table := range []string{"projects", "sandboxes", "sandbox_provider_instances", "workers", "project_events", "jobqueue_jobs"} {
		if !tenantDB.Write.Migrator().HasTable(table) {
			t.Fatalf("tenant schema missing table %s", table)
		}
	}
	for _, table := range []string{"tenants", "users"} {
		if tenantDB.Write.Migrator().HasTable(table) {
			t.Fatalf("tenant schema unexpectedly has global table %s", table)
		}
	}
}

func TestResolverCachesSQLiteTenantDatabase(t *testing.T) {
	ctx := context.Background()
	resolver := database.NewResolver(database.ResolverConfig{
		Config: database.Config{
			Driver: gormdb.DriverSQLite,
			DSN:    "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db"),
		},
	})
	t.Cleanup(func() {
		if err := resolver.Close(); err != nil {
			t.Fatalf("close resolver: %v", err)
		}
	})

	db1, err := resolver.Resolve(ctx, "tenant-1")
	if err != nil {
		t.Fatalf("resolve tenant-1 first: %v", err)
	}
	db2, err := resolver.Resolve(ctx, "tenant-1")
	if err != nil {
		t.Fatalf("resolve tenant-1 second: %v", err)
	}
	if db1 != db2 {
		t.Fatalf("same tenant resolved to different sqlite DBs")
	}
}

func fileExists(path string) bool {
	matches, err := filepath.Glob(path)
	return err == nil && len(matches) == 1
}
