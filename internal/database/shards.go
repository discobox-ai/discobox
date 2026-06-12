package database

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/gormdb"
)

var safeTenantID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Resolver opens and caches database connections by tenant ID.
//
// Postgres currently resolves every tenant to the same DB because tenancy is
// represented in rows. SQLite resolves each tenant to its own database file by
// inserting the tenant ID into the configured SQLite file name.
type Resolver struct {
	cfg Config

	mu      sync.Mutex
	global  *DB
	shards  map[string]*DB
	migrate bool
}

// ResolverConfig controls NewResolver.
type ResolverConfig struct {
	Config Config

	// MigrateOnOpen runs migrations the first time a tenant database is opened.
	MigrateOnOpen bool
}

// NewResolver creates a database resolver for tenant-sharded databases.
func NewResolver(cfg ResolverConfig) *Resolver {
	return &Resolver{
		cfg:     cfg.Config,
		shards:  map[string]*DB{},
		migrate: cfg.MigrateOnOpen,
	}
}

// Resolve returns the database connection for tenantID.
func (r *Resolver) Resolve(ctx context.Context, tenantID string) (*DB, error) {
	if r == nil {
		return nil, fmt.Errorf("database resolver is nil")
	}
	if tenantID == "" {
		return nil, fmt.Errorf("tenant ID is required")
	}
	if !safeTenantID.MatchString(tenantID) {
		return nil, fmt.Errorf("tenant ID %q is not safe for database resolution", tenantID)
	}

	switch r.cfg.Driver {
	case gormdb.DriverPostgres:
		return r.resolvePostgresTenant(ctx, tenantID)
	case gormdb.DriverSQLite:
		return r.resolveSQLite(ctx, tenantID)
	default:
		return nil, fmt.Errorf("unsupported database driver: %s", r.cfg.Driver)
	}
}

// ResolveGlobal returns the global database connection. The global schema holds
// non-tenant tables only.
func (r *Resolver) ResolveGlobal(ctx context.Context) (*DB, error) {
	if r == nil {
		return nil, fmt.Errorf("database resolver is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.global != nil {
		return r.global, nil
	}
	db, err := New(r.cfg)
	if err != nil {
		return nil, err
	}
	if r.migrate {
		if err := db.MigrateGlobal(ctx); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	r.global = db
	return db, nil
}

// ResolveTenantDB resolves tenant-scoped write/read GORM handles for store.Store.
func (r *Resolver) ResolveTenantDB(ctx context.Context, tenantID string) (write, read *gorm.DB, err error) {
	db, err := r.Resolve(ctx, tenantID)
	if err != nil {
		return nil, nil, err
	}
	read = db.Read
	if read == nil {
		read = db.Write
	}
	return db.Write, read, nil
}

// Close closes all opened database connections.
func (r *Resolver) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.global != nil {
		if err := r.global.Close(); err != nil {
			return err
		}
		r.global = nil
	}
	for tenantID, db := range r.shards {
		if err := db.Close(); err != nil {
			return err
		}
		delete(r.shards, tenantID)
	}
	return nil
}

func (r *Resolver) resolvePostgresTenant(ctx context.Context, tenantID string) (*DB, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if db := r.shards[tenantID]; db != nil {
		return db, nil
	}
	schema := PostgresTenantSchema(tenantID)
	global, err := r.resolveGlobalLocked(ctx)
	if err != nil {
		return nil, err
	}
	if err := global.Write.WithContext(ctx).Exec("CREATE SCHEMA IF NOT EXISTS " + schema).Error; err != nil {
		return nil, err
	}
	cfg := r.cfg
	cfg.DSN = PostgresTenantDSN(r.cfg.DSN, schema)
	cfg.ReadDSN = PostgresTenantDSN(r.cfg.ReadDSN, schema)
	db, err := New(cfg)
	if err != nil {
		return nil, err
	}
	if r.migrate {
		if err := db.MigrateTenant(ctx); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	r.shards[tenantID] = db
	return db, nil
}

func (r *Resolver) resolveSQLite(ctx context.Context, tenantID string) (*DB, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if db := r.shards[tenantID]; db != nil {
		return db, nil
	}
	cfg := r.cfg
	cfg.DSN = TenantSQLiteDSN(r.cfg.DSN, tenantID)
	cfg.ReadDSN = ""
	db, err := New(cfg)
	if err != nil {
		return nil, err
	}
	if r.migrate {
		if err := db.MigrateTenant(ctx); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	r.shards[tenantID] = db
	return db, nil
}

func (r *Resolver) resolveGlobalLocked(ctx context.Context) (*DB, error) {
	if r.global != nil {
		return r.global, nil
	}
	db, err := New(r.cfg)
	if err != nil {
		return nil, err
	}
	if r.migrate {
		if err := db.MigrateGlobal(ctx); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	r.global = db
	return db, nil
}

// TenantSQLiteDSN returns a tenant-specific SQLite DSN derived from baseDSN.
//
// For file-backed SQLite, the tenant ID is inserted into the file name:
// sqlite3:///data/discobox.db + tenant-1 => sqlite3:///data/discobox.tenant-1.db
//
// In-memory SQLite cannot be file-sharded, so it is returned unchanged.
func TenantSQLiteDSN(baseDSN, tenantID string) string {
	prefix := ""
	raw := baseDSN
	for _, candidate := range []string{"sqlite3://", "sqlite://"} {
		if strings.HasPrefix(raw, candidate) {
			prefix = candidate
			raw = strings.TrimPrefix(raw, candidate)
			break
		}
	}
	if raw == ":memory:" || strings.HasPrefix(raw, ":memory:") {
		return baseDSN
	}

	filePrefix := ""
	if strings.HasPrefix(raw, "file:") {
		filePrefix = "file:"
		raw = strings.TrimPrefix(raw, "file:")
	}

	dir := filepath.Dir(raw)
	base := filepath.Base(raw)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	if name == "" || name == "." {
		name = "tenant"
	}
	sharded := filepath.Join(dir, name+"."+tenantID+ext)
	return prefix + filePrefix + sharded
}

// PostgresTenantSchema maps a tenant ID to a safe Postgres schema name.
func PostgresTenantSchema(tenantID string) string {
	var b strings.Builder
	b.WriteString("tenant_")
	for _, r := range tenantID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// PostgresTenantDSN returns a DSN configured with a tenant schema search_path.
func PostgresTenantDSN(baseDSN, schema string) string {
	if baseDSN == "" {
		return ""
	}
	if u, err := url.Parse(baseDSN); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String()
	}
	if strings.Contains(baseDSN, "search_path=") {
		return baseDSN
	}
	return strings.TrimSpace(baseDSN) + " search_path=" + schema
}
