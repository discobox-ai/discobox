// Package database opens and migrates application database connections.
package database

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/x/gormdb"
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
	// Runs before AutoMigrate, unlike every other step here: it clears the way
	// for a unique index AutoMigrate is about to create, and creating that
	// index is what fails on duplicate data.
	if err := deduplicateSandboxNames(write); err != nil {
		return err
	}
	// Also pre-AutoMigrate, for the same shape of reason: AutoMigrate applies
	// runtime_state's NOT NULL by rebuilding the table, and the rebuild fails
	// on the rows it would have to repair.
	if err := prepareSandboxStateSplit(write); err != nil {
		return err
	}
	// And again pre-AutoMigrate: AutoMigrate creates a missing index but never
	// alters one that already exists under the same name, so an index whose
	// column set changed has to be dropped first and recreated by it.
	if err := dropNarrowSandboxSecretEnvIndex(write); err != nil {
		return err
	}
	// Pre-AutoMigrate as well, and for the sharpest reason of the four: this is
	// a rename, and AutoMigrate cannot tell one from an addition. Left to it,
	// it would add max_grant_ttl_seconds at its default and leave every
	// configured limit behind in the old column — every secret silently reset
	// to an hour, with the evidence still in the table.
	if err := renameSecretGrantTTLColumn(write); err != nil {
		return err
	}
	if err := write.AutoMigrate(model.AllModels()...); err != nil {
		return err
	}
	if err := widenSecretUniquenessIndex(write); err != nil {
		return err
	}
	if err := migrateSecretTypes(write); err != nil {
		return err
	}
	if err := normalizeSecretHosts(write); err != nil {
		return err
	}
	if err := liftConfiguredSecretGrantLimits(write); err != nil {
		return err
	}
	if err := migrateLibkrunProviderType(write); err != nil {
		return err
	}
	if err := migrateMacOSProviderType(write); err != nil {
		return err
	}
	if err := dropLegacyPoolBootstrapTokenConstraint(write); err != nil {
		return err
	}
	if err := dropJobQueueArtifacts(write); err != nil {
		return err
	}
	if err := dropLegacyProjectSlug(write); err != nil {
		return err
	}
	if err := dropSandboxResourceRequestColumns(write); err != nil {
		return err
	}
	if err := dropProjectEvents(write); err != nil {
		return err
	}
	return migrateSandboxStateSplit(write)
}

// prepareSandboxStateSplit makes the sandboxes table safe for AutoMigrate to
// apply the ADR 0034 schema to, and must run before it.
//
// `runtime_state` predates the split as a nullable provider blob and is now a
// NOT NULL string. Applying that constraint is a table rebuild on SQLite, and
// the rebuild fails outright on any row holding NULL — which is every sandbox
// whose provider never recorded state. AutoMigrate cannot fix that itself,
// because the rows it would have to repair are the ones stopping it, so the
// NULLs have to be gone before it runs.
//
// Everything else about the split waits until after AutoMigrate, in
// migrateSandboxStateSplit, where the columns it needs exist.
func prepareSandboxStateSplit(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasTable("sandboxes") || !migrator.HasColumn(&model.Sandbox{}, "runtime_state") {
		return nil // fresh database: AutoMigrate creates the column correctly
	}
	return db.Exec(`UPDATE sandboxes SET runtime_state = '' WHERE runtime_state IS NULL`).Error
}

// migrateSandboxStateSplit moves a pre-ADR-0034 sandbox row onto the two state
// fields, and moves the provider blob out of the column the runtime axis now
// owns.
//
// Each statement is guarded by exactly what it is looking for, so the sequence
// is idempotent and safe to resume after an interrupted run. Order is
// load-bearing: the blob has to leave `runtime_state` before a power value is
// written into it, or a rerun would find a state string where it expected JSON.
func migrateSandboxStateSplit(db *gorm.DB) error {
	// A legacy provider blob is anything the runtime vocabulary does not
	// contain, empty included. Matching the closed set is what makes the pass
	// idempotent — after it runs, every value in the column is a member — and
	// it is the only test that works on the value as stored: GORM wrote the
	// blob as a []byte, so SQLite holds it with BLOB storage class, and a
	// `LIKE '{%'` sniff silently matches none of them.
	runtimeValues := append([]string{""}, model.SandboxRuntimeStates...)
	statements := []struct {
		sql  string
		args []any
	}{
		{
			sql: `UPDATE sandboxes SET provider_state = runtime_state
			      WHERE (provider_state IS NULL OR provider_state = '') AND runtime_state NOT IN (?)`,
			args: []any{runtimeValues},
		},
		{
			sql:  `UPDATE sandboxes SET runtime_state = '' WHERE runtime_state NOT IN (?)`,
			args: []any{runtimeValues},
		},
		// A power value in `state` predates the split. It moves to the runtime
		// axis and leaves `ready` behind: such a row described a sandbox whose
		// container existed and had been converged, which is what `ready` now
		// says. The old anchor described that power transition, so it moves
		// with it; `state_changed_at` stays put, and nothing derives a deadline
		// from `ready`.
		{
			sql: `UPDATE sandboxes
			      SET runtime_state = state, runtime_state_changed_at = state_changed_at, state = ?
			      WHERE state IN (?)`,
			args: []any{model.SandboxStateReady, model.SandboxRuntimeStates},
		},
	}
	for _, statement := range statements {
		if err := db.Exec(statement.sql, statement.args...).Error; err != nil {
			return err
		}
	}
	return nil
}

const (
	legacyLibkrunProviderType = "local-vm"
	libkrunProviderType       = "libkrun"

	placeholderMacOSProviderType = "macos"
	vzProviderType               = "vz"

	legacyPoolBootForeignKey  = "fk_pools_bootstrap_tokens"
	poolBootCascadeForeignKey = "fk_pool_bootstrap_tokens_pool"

	//nolint:gosec // An index name, not a credential.
	sandboxSecretEnvIndex = "idx_sandbox_secret_env"
	//nolint:gosec // An index name, not a credential.
	secretUniquenessIndex = "idx_secret_project_type_host"
)

// dropNarrowSandboxSecretEnvIndex removes the pre-ADR-0031 two-column
// (sandbox_id, env_name) uniqueness index on sandbox_secrets so AutoMigrate can
// recreate it over the three columns it now covers. Agent-requested bindings
// are a separate channel that is never injected into the sandbox, so a sandbox
// may hold one injected and one agent-requested binding for the same
// environment variable; under the narrow index the second one fails to insert.
//
// "The column is absent" is the exact test for "this database predates the
// change", because the column and the widened index arrive together — and it
// needs no dialect-specific index introspection. The check is cheap and
// idempotent, so it stays as a permanent step rather than a one-shot.
func dropNarrowSandboxSecretEnvIndex(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasTable(&model.SandboxSecret{}) {
		return nil
	}
	if migrator.HasColumn(&model.SandboxSecret{}, "agent_requested") {
		return nil
	}
	if !migrator.HasIndex(&model.SandboxSecret{}, sandboxSecretEnvIndex) {
		return nil
	}
	return migrator.DropIndex(&model.SandboxSecret{}, sandboxSecretEnvIndex)
}

// renameSecretGrantTTLColumn carries a secret's grant lifetime from
// default_grant_ttl_seconds to max_grant_ttl_seconds, which is the same number
// under a name that says what it does.
//
// It was a default: the lifetime a grant took when its minter named none, and
// nothing stopped that minter from asking for longer, or for a grant that never
// expired. It is now the ceiling as well, so the value a database already
// carries keeps its meaning and gains teeth. Renaming rather than adding is
// what makes that true of existing rows.
func renameSecretGrantTTLColumn(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasTable(&model.Secret{}) {
		return nil
	}
	if !migrator.HasColumn(&model.Secret{}, "default_grant_ttl_seconds") {
		return nil
	}
	if migrator.HasColumn(&model.Secret{}, "max_grant_ttl_seconds") {
		// Both columns present: an interrupted run already added the new one,
		// so the old is stale and AutoMigrate owns the rest.
		return nil
	}
	return db.Exec(`ALTER TABLE "secrets" RENAME COLUMN "default_grant_ttl_seconds" TO "max_grant_ttl_seconds"`).Error
}

// widenSecretUniquenessIndex drops the secret uniqueness index when it predates
// the name being part of it, so AutoMigrate recreates it over the columns it
// now covers.
//
// The old domain was (project, type, host, unique_key), which allowed a project
// one secret per type and host. Nothing infers a host any more, so that became
// one unbound token per project — a GitHub token and an OpenAI key could not
// coexist, and the collision surfaced as a constraint violation rather than an
// answer. The index is read rather than guessed at: it is the columns, not a
// version, that say whether this database has the old shape.
func widenSecretUniquenessIndex(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasTable(&model.Secret{}) || !migrator.HasIndex(&model.Secret{}, secretUniquenessIndex) {
		return nil
	}
	indexes, err := migrator.GetIndexes(&model.Secret{})
	if err != nil {
		return err
	}
	for _, index := range indexes {
		if index.Name() != secretUniquenessIndex {
			continue
		}
		if slices.Contains(index.Columns(), "name") {
			return nil
		}
		return migrator.DropIndex(&model.Secret{}, secretUniquenessIndex)
	}
	return nil
}

// migrateSecretTypes brings stored secrets onto the two types that exist:
// token and oauth.
//
// "bearer" is renamed — the proxy swaps a value into whatever header the
// sandbox put it in, so naming the type after one HTTP scheme named a
// requirement that was never enforced. "git" and "ssh" are deleted, with the
// grants, requests, and sandbox bindings that stand on them: cleartext leaves
// the control plane only through ResolveSandboxSecret, which emits the token
// alone, so a git or ssh secret never resolved into a sandbox and nothing that
// worked is being taken away.
//
// It runs before AutoMigrate for a reason the enum makes sharp: the API
// validates it on the way out as well as in, so a single "git" row left behind
// would fail to serialize and take the whole secret listing with it.
func migrateSecretTypes(db *gorm.DB) error {
	if !db.Migrator().HasTable("secrets") {
		return nil
	}
	if err := db.Exec("UPDATE secrets SET type = 'token' WHERE type = 'bearer'").Error; err != nil {
		return err
	}
	if db.Migrator().HasTable("secret_requests") {
		if err := db.Exec("UPDATE secret_requests SET type = 'token' WHERE type = 'bearer'").Error; err != nil {
			return err
		}
		if err := db.Exec("DELETE FROM secret_requests WHERE type IN ('git','ssh')").Error; err != nil {
			return err
		}
	}
	// Everything standing on an unusable secret goes with it, deepest first:
	// a grant or a binding whose secret is gone is a row that can only produce
	// a lookup failure.
	for _, table := range []string{"sandbox_secrets", "harness_config_secret_bindings", "secret_grants"} {
		if !db.Migrator().HasTable(table) {
			continue
		}
		if err := db.Exec("DELETE FROM " + table + " WHERE secret_id IN (SELECT id FROM secrets WHERE type IN ('git','ssh'))").Error; err != nil {
			return err
		}
	}
	return db.Exec("DELETE FROM secrets WHERE type IN ('git','ssh')").Error
}

// normalizeSecretHosts lowercases the destination hosts already stored on
// secrets, requests, and grants.
//
// A grant is matched against the host the proxy observed, which it reports
// lowercased, by SQL equality. So a row written with any other casing is an
// approval nothing can ever use, and the symptom is a credential that behaves
// as if it were revoked. New writes are normalized by the secrets service; this
// repairs the rows written before it was.
//
// It is a permanent, idempotent step rather than a one-shot: the WHERE clause
// matches nothing once the data is clean, and it costs one indexless scan of
// three small tables at startup.
func normalizeSecretHosts(db *gorm.DB) error {
	for _, table := range []string{"secrets", "secret_requests", "secret_grants"} {
		if !db.Migrator().HasTable(table) {
			continue
		}
		if err := db.Exec("UPDATE " + table + " SET host = lower(host) WHERE host <> lower(host)").Error; err != nil {
			return err
		}
	}
	return nil
}

// liftConfiguredSecretGrantLimits takes the ceiling off the credentials a
// harness configure flow created.
//
// Those secrets are the harness's own: the flow binds one, grants it at harness
// scope with no expiry, and deletes it when the harness is deconfigured. The
// grant never lapses, because a harness that stops working an hour after it was
// configured is a harness nobody configured. The limit these rows carry was
// never chosen — it was the column default, back when the number was only a
// default — and under a ceiling it would describe a lifetime the credential's
// own grant does not have and refuse the next grant somebody writes for it.
//
// Only rows still carrying that old default are touched, and only those the
// configure flow created. A secret a person created and then bound to a harness
// keeps whatever limit that person set; so does a configured secret whose limit
// somebody has since changed.
func liftConfiguredSecretGrantLimits(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasTable(&model.HarnessConfig{}) || !migrator.HasTable(&model.Secret{}) {
		return nil
	}
	var configs []model.HarnessConfig
	if err := db.Select("id", "configured_secret_ids").Find(&configs).Error; err != nil {
		return err
	}
	var ids []string
	for _, config := range configs {
		for _, secretID := range config.ConfiguredSecretIDs {
			if secretID = strings.TrimSpace(secretID); secretID != "" {
				ids = append(ids, secretID)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return db.Model(&model.Secret{}).
		Where("id IN ? AND max_grant_ttl_seconds = ?", ids, defaultGrantTTLSecondsBeforeItWasACeiling).
		Update("max_grant_ttl_seconds", 0).Error
}

// defaultGrantTTLSecondsBeforeItWasACeiling is the hour every secret carried
// when the column was a default nobody set. It is the evidence that a row's
// limit was never chosen, and it is only ever read as that.
const defaultGrantTTLSecondsBeforeItWasACeiling = 3600

// migrateLibkrunProviderType preserves provider and pool identities while
// replacing the pre-release local-vm backend name with its implementation name.
func migrateLibkrunProviderType(db *gorm.DB) error {
	return db.Model(&model.SandboxProviderInstance{}).
		Where("type = ?", legacyLibkrunProviderType).
		Update("type", libkrunProviderType).Error
}

// migrateMacOSProviderType replaces the placeholder installed on macOS before a
// backend existed there.
//
// "macos" was never a registered provider type: the seed created it disabled so
// that a Mac had a default provider row and a Default pool to bind to, and both
// were inert. Rewriting the type in place — rather than seeding a new instance —
// keeps that Default pool, and any pool a user pointed at it, working against
// the backend that now exists. The instance is enabled for the same reason: it
// was only ever disabled because it could not run anything.
func migrateMacOSProviderType(db *gorm.DB) error {
	return db.Model(&model.SandboxProviderInstance{}).
		Where("type = ?", placeholderMacOSProviderType).
		Updates(map[string]any{"type": vzProviderType, "disabled": false}).Error
}

// dropLegacyPoolBootstrapTokenConstraint completes the upgrade from the
// original restrictive pool-token foreign key. AutoMigrate creates the newly
// named cascading constraint first; only then is it safe to remove the old
// constraint. Using GORM's migrator keeps the same migration valid for SQLite
// and PostgreSQL.
func dropLegacyPoolBootstrapTokenConstraint(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasConstraint("pool_bootstrap_tokens", legacyPoolBootForeignKey) {
		return nil
	}
	if !migrator.HasConstraint("pool_bootstrap_tokens", poolBootCascadeForeignKey) {
		return fmt.Errorf("pool bootstrap token cascade constraint %q is missing", poolBootCascadeForeignKey)
	}
	return migrator.DropConstraint("pool_bootstrap_tokens", legacyPoolBootForeignKey)
}

// dropJobQueueArtifacts removes the retired job-queue schema: the job and
// leader tables, and the last_job_id lifecycle columns. Reconciliation now
// rides the reconcile engine's dirty set.
func dropJobQueueArtifacts(db *gorm.DB) error {
	drop := func(tx *gorm.DB) error {
		for _, table := range []string{"jobqueue_jobs", "jobqueue_leaders"} {
			if tx.Migrator().HasTable(table) {
				if err := tx.Migrator().DropTable(table); err != nil {
					return err
				}
			}
		}
		for _, m := range []any{&model.Sandbox{}} {
			if tx.Migrator().HasColumn(m, "last_job_id") {
				if err := tx.Migrator().DropColumn(m, "last_job_id"); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if db.Name() != "sqlite" {
		return drop(db)
	}
	// SQLite drops columns by rebuilding the table (create new, copy, DROP TABLE,
	// rename). With foreign_keys ON, dropping `workers` fails because sandboxes
	// and bootstrap tokens reference it. Follow SQLite's documented ALTER TABLE
	// procedure: disable the pragma around the rebuild. Pragmas are
	// per-connection, so pin one connection for the whole operation.
	return db.Connection(func(tx *gorm.DB) error {
		if err := tx.Exec("PRAGMA foreign_keys = OFF").Error; err != nil {
			return err
		}
		defer tx.Exec("PRAGMA foreign_keys = ON")
		return drop(tx)
	})
}

// dropLegacyProjectSlug removes the retired projects.slug column (ADR 0023 §5).
// AutoMigrate adds columns but never drops them, so a database created before
// the column was retired keeps it — declared NOT NULL with no default, which
// fails every project write the moment the model stops populating it.
//
// The rebuild is the same SQLite dance dropJobQueueArtifacts documents: the
// projects table is referenced by most of the schema, so the foreign-key pragma
// comes off around it.
func dropLegacyProjectSlug(db *gorm.DB) error {
	return dropRetiredColumn(db, &model.Project{}, "projects", "slug")
}

// dropSandboxResourceRequestColumns removes the retired per-sandbox
// cpu_vcpus/memory_bytes/storage_bytes request columns (docs/adr/0029):
// sandboxes no longer reserve a slice of their pool's envelope, so nothing
// writes these columns anymore. The same-named columns on pools are the
// envelope itself and are untouched.
func dropSandboxResourceRequestColumns(db *gorm.DB) error {
	for _, column := range []string{"cpu_vcpus", "memory_bytes", "storage_bytes"} {
		if err := dropRetiredColumn(db, &model.Sandbox{}, "sandboxes", column); err != nil {
			return err
		}
	}
	return nil
}

// dropProjectEvents removes the retired project_events table (ADR 0081).
//
// AutoMigrate never drops a table, so a database created before the model was
// retired keeps the rows and their six indexes, and goes on paying for neither.
// The history is discarded rather than migrated anywhere: nothing has read it
// since ADR 0061 removed the stream, and no reader was ever built for it.
//
// A plain drop needs no foreign-key dance. project_events references projects,
// but nothing references project_events, so there is no row elsewhere whose
// constraint the drop could break.
func dropProjectEvents(db *gorm.DB) error {
	if !db.Migrator().HasTable("project_events") {
		return nil
	}
	return db.Migrator().DropTable("project_events")
}

// dropRetiredColumn removes a column that is no longer part of a model.
//
// It issues the DDL directly rather than through Migrator().DropColumn. On
// SQLite that helper rebuilds the table by rewriting its stored CREATE TABLE
// text, and it only matches the column when the DDL quotes the identifier the
// way GORM writes it. A column added by anything else is left in place and the
// call still reports success. `ALTER TABLE ... DROP COLUMN` has no such
// dependency and is supported by both SQLite (3.35+) and PostgreSQL.
//
// The foreign-key pragma comes off around the statement for the same reason
// dropJobQueueArtifacts documents: SQLite implements the drop by rebuilding the
// table, which trips references from other tables. Pragmas are per-connection,
// so the rebuild is pinned to one connection.
func dropRetiredColumn(db *gorm.DB, model any, table, column string) error {
	drop := func(tx *gorm.DB) error {
		if !tx.Migrator().HasColumn(model, column) {
			return nil
		}
		if tx.Name() == "sqlite" {
			// SQLite's ALTER TABLE DROP COLUMN refuses a column that is still
			// covered by an index: a database created before the column was
			// retired can carry one (e.g. a uniqueIndex struct tag from when
			// the field still existed), and AutoMigrate never drops indexes
			// any more than it drops columns. Postgres has no such
			// restriction — dropping a column there drops its indexes too.
			if err := dropIndexesCoveringColumn(tx, table, column); err != nil {
				return err
			}
		}
		return tx.Exec(fmt.Sprintf("ALTER TABLE %q DROP COLUMN %q", table, column)).Error
	}
	if db.Name() != "sqlite" {
		return drop(db)
	}
	return db.Connection(func(tx *gorm.DB) error {
		if err := tx.Exec("PRAGMA foreign_keys = OFF").Error; err != nil {
			return err
		}
		defer tx.Exec("PRAGMA foreign_keys = ON")
		return drop(tx)
	})
}

// dropIndexesCoveringColumn drops every SQLite index that references column,
// so a subsequent ALTER TABLE DROP COLUMN is not refused. Indexes whose name
// starts with sqlite_ back an inline PRIMARY KEY/UNIQUE constraint rather than
// a real CREATE INDEX statement; SQLite does not allow dropping those
// directly, and a retired application column is never part of one.
func dropIndexesCoveringColumn(tx *gorm.DB, table, column string) error {
	var indexes []struct {
		Name string `gorm:"column:name"`
	}
	if err := tx.Raw(fmt.Sprintf("PRAGMA index_list(%q)", table)).Scan(&indexes).Error; err != nil {
		return err
	}
	for _, index := range indexes {
		if strings.HasPrefix(index.Name, "sqlite_") {
			continue
		}
		var columns []struct {
			Name string `gorm:"column:name"`
		}
		if err := tx.Raw(fmt.Sprintf("PRAGMA index_info(%q)", index.Name)).Scan(&columns).Error; err != nil {
			return err
		}
		for _, c := range columns {
			if c.Name != column {
				continue
			}
			if err := tx.Exec(fmt.Sprintf("DROP INDEX %q", index.Name)).Error; err != nil {
				return err
			}
			break
		}
	}
	return nil
}

// Close closes the underlying database pools.
func (db *DB) Close() error {
	if db == nil || db.pools == nil {
		return nil
	}
	return db.pools.Close()
}

// deduplicateSandboxNames renames sandboxes that share a name within a project
// so the idx_sandbox_project_name unique index can be created. Names became
// unique only once they were promoted to an addressable handle (an ssh_config
// Host alias, per cli/DESIGN.md), and nothing stopped duplicates before that.
//
// Renaming rather than refusing to start is the deliberate choice: a sandbox is
// live state a user may have work inside, so a duplicate name must not be able
// to strand a server on startup. The oldest holder keeps the name — it is the
// one whose name the user has been using — and every later one is suffixed with
// its own sandbox ID, which is unique by construction. In the vanishingly
// unlikely case that suffixed name is itself taken, a counter is appended, so
// this always terminates with a set of unique names.
func deduplicateSandboxNames(db *gorm.DB) error {
	if !db.Migrator().HasTable(&model.Sandbox{}) {
		return nil
	}
	type sandboxName struct {
		ID        string
		ProjectID string
		Name      string
	}
	var sandboxes []sandboxName
	if err := db.Model(&model.Sandbox{}).
		Select("id", "project_id", "name").
		Order("project_id, created_at, id").
		Find(&sandboxes).Error; err != nil {
		return fmt.Errorf("read sandbox names: %w", err)
	}

	taken := map[string]bool{}
	for _, sandbox := range sandboxes {
		taken[sandbox.ProjectID+"\x00"+sandbox.Name] = true
	}
	seen := map[string]bool{}
	for _, sandbox := range sandboxes {
		key := sandbox.ProjectID + "\x00" + sandbox.Name
		if !seen[key] {
			seen[key] = true
			continue
		}
		renamed := sandbox.Name + "-" + sandbox.ID
		for counter := 2; taken[sandbox.ProjectID+"\x00"+renamed]; counter++ {
			renamed = fmt.Sprintf("%s-%s-%d", sandbox.Name, sandbox.ID, counter)
		}
		taken[sandbox.ProjectID+"\x00"+renamed] = true
		seen[sandbox.ProjectID+"\x00"+renamed] = true
		if err := db.Model(&model.Sandbox{}).
			Where("id = ?", sandbox.ID).
			Update("name", renamed).Error; err != nil {
			return fmt.Errorf("rename duplicate sandbox %s: %w", sandbox.ID, err)
		}
		log.Printf("renamed sandbox %s from %q to %q: sandbox names are now unique within a project", sandbox.ID, sandbox.Name, renamed)
	}
	return nil
}
