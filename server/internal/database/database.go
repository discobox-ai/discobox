// Package database opens and migrates application database connections.
package database

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/discobox-ai/discobox/pkg/hostscope"
	"github.com/discobox-ai/discobox/pkg/platform"
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
	// Read before AutoMigrate adds the columns, which is the one moment a row
	// with no image kind is known to be one from before kinds.
	backfillPoolKinds := addsImageKindColumn(write, &model.Pool{})
	if err := write.AutoMigrate(model.AllModels()...); err != nil {
		return err
	}
	if backfillPoolKinds {
		if err := backfillPoolImageKinds(write); err != nil {
			return err
		}
	}
	if err := unsetManifestFileImageKinds(write); err != nil {
		return err
	}
	if err := respellDiscoVMImageKinds(write); err != nil {
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
	if err := moveSecretHostsToLists(write); err != nil {
		return err
	}
	if err := liftConfiguredSecretGrantLimits(write); err != nil {
		return err
	}
	if err := retrustConfiguredCodexWorkingDir(write); err != nil {
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
	if err := dropPoolImageStaging(write); err != nil {
		return err
	}
	if err := renameSandboxEgressReconcile(write); err != nil {
		return err
	}
	if err := dropProjectEvents(write); err != nil {
		return err
	}
	if err := migrateSandboxStateSplit(write); err != nil {
		return err
	}
	if err := normalizeCredentialVerdictTimes(write); err != nil {
		return err
	}
	if err := backfillSecretValueUpdatedAt(write); err != nil {
		return err
	}
	if err := migrateCredentialVerdictNeeds(write); err != nil {
		return err
	}
	if err := deleteOrphanedSecretGrants(write); err != nil {
		return err
	}
	if err := denyOrphanedSecretRequests(write); err != nil {
		return err
	}
	if err := retireApprovalRuleUses(write); err != nil {
		return err
	}
	if err := retireAgentGrantsWithoutPolicy(write); err != nil {
		return err
	}
	if err := retireHarnessImages(write); err != nil {
		return err
	}
	return rekeySandboxOrigins(write)
}

// retireHarnessImages takes the runnable images off the harness configs that
// recorded one (ADR 26-10-16-423 §10): no harness is an image any more, and a
// harness config's image is a manifest file's or nothing. A config on a
// bundle keeps its bundle, which is what every box on it applies, and loses
// the twin it recorded beside it, with the platforms read from that twin's
// index, which a bundle's steps select instead. A config from before bundles
// loses the image it ran as and keeps the manifest its labels gave it: it
// runs nowhere until a bundle is registered for it, and says so.
//
// A sandbox's own image pin is not cleared here: the pool still keeps a
// container a sandbox from before bundles runs, by the fingerprint that pin
// is part of, and the sandbox leaves it when it is pinned to its bundles
// (sandboxes.Service.PinUndeliveredSandboxes).
//
// It runs on every start, and once nothing is left to clear it updates
// nothing: a manifest file's image is the one image it leaves.
func retireHarnessImages(db *gorm.DB) error {
	var configs []model.HarnessConfig
	retired := db.Model(&model.HarnessConfig{}).Where("image <> ? AND image NOT LIKE ?", "", "file://%")
	if err := retired.Select("id", "project_id", "slug", "bundle", "image", "image_digest").Find(&configs).Error; err != nil {
		return err
	}
	if len(configs) == 0 {
		return nil
	}
	// Each is logged with the image it ran as, which is the only record left
	// of what its owner registered.
	for _, config := range configs {
		log.Printf("cleared harness config %s (%s) of project %s of the runnable image it ran as, %s@%s (bundle %q): no harness is an image any more, and one with no bundle runs nowhere until one is registered for it",
			config.ID, config.Slug, config.ProjectID, config.Image, config.ImageDigest, config.Bundle)
	}
	// UpdateColumns, not Updates: a migration is not an edit, and leaves
	// updated_at as it was.
	return db.Model(&model.HarnessConfig{}).Where("image <> ? AND image NOT LIKE ?", "", "file://%").
		UpdateColumns(map[string]any{"image": "", "image_digest": "", "platforms": nil}).Error
}

// retireApprovalRuleUses takes the approval rules no code can read out of the
// database (ADR 26-10-22-689 §5): a rule now carries a policy a request's
// policy is held to, and no code reads a sentence as one. Every rule with no
// policy is deleted, as DeleteApprovalRule deletes one: the grants it minted
// stand, and they and its verdicts go on naming it by ID. Each is logged with
// what it said, which is the only record left of it; people make the ones
// they want again as policies. A rule with a policy is never touched, so an
// older server that adds a uses column back — a rollback, a rolling deploy —
// costs no rule made since, and a rule it writes with no policy is deleted by
// the next start of this one.
//
// The rules' older columns go once their rules have: the uses, and the scope
// sentence and pinned discobox (ADR 26-10-12-330 §2) from a branch that never
// reached a release, with the scope verdicts, which read the scope sentences
// and are of a kind the API no longer names.
//
// A step interrupted before the drops and run again finds the same rows gone
// and finishes. Once there is nothing to delete and no column to drop, it
// does one read.
func retireApprovalRuleUses(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasTable(&model.ApprovalRule{}) {
		return nil
	}
	var retired []string
	for _, column := range []string{"uses", "scope", "sandbox_id"} {
		if migrator.HasColumn(&model.ApprovalRule{}, column) {
			retired = append(retired, column)
		}
	}
	var rules []map[string]any
	if err := db.Table("approval_rules").
		Select(append([]string{"id", "project_id", "well_known_id", "secret_id"}, retired...)).
		Where("policy IS NULL OR policy = '' OR policy = 'null'").Order("id").Find(&rules).Error; err != nil {
		return err
	}
	for _, rule := range rules {
		credential := columnText(rule["well_known_id"])
		if credential == "" {
			credential = "secret " + columnText(rule["secret_id"])
		}
		said := make([]string, 0, len(retired))
		for _, column := range retired {
			// %q: each is what a person or a model wrote.
			said = append(said, fmt.Sprintf("%s %q", column, columnText(rule[column])))
		}
		log.Printf("deleted approval rule %s of project %s for %s (%s): a rule now carries a policy, and is made anew as one; its grants stand",
			columnText(rule["id"]), columnText(rule["project_id"]), credential, strings.Join(said, ", "))
		if err := db.Exec("DELETE FROM approval_rules WHERE id = ?", rule["id"]).Error; err != nil {
			return err
		}
	}
	if len(retired) == 0 {
		return nil
	}
	// Before the columns go: they are what says this step has work left, so
	// a start interrupted after them would leave the verdicts for good.
	if migrator.HasTable(&model.CredentialVerdict{}) {
		if err := db.Exec("DELETE FROM credential_verdicts WHERE kind = 'scope'").Error; err != nil {
			return err
		}
	}
	for _, column := range retired {
		if err := dropRetiredColumn(db, &model.ApprovalRule{}, "approval_rules", column); err != nil {
			return err
		}
	}
	return nil
}

// retireAgentGrantsWithoutPolicy takes out what was approved, or asked for,
// as uses alone (#312, ADR 26-10-20-782 §11): every grant an agent takes
// through discobox-access, and every delegation, now carries a policy, and a
// hand-off is bound by its delegation's. An agent grant — one with uses, or a
// delegation — with no policy is deleted, as revoking it deletes it: its
// request stays approved and reads as denied, and its verdicts go on naming
// it. A pending agent request with no policy is deleted too, since nothing can
// approve it; the agent waiting on it is told it is gone and asks again with
// a policy. Each is logged with what it said.
//
// Standing grants, with no uses, are never touched: the harness-config grants
// `discobox configure` writes and a person's `secret grant create` without
// uses authorize a sentinel the discobox already holds, which no policy
// narrows, and a harness resolves its secrets only through them.
//
// It runs on every start, and finds nothing once nothing writes such a row.
func retireAgentGrantsWithoutPolicy(db *gorm.DB) error {
	migrator := db.Migrator()
	const noPolicy = "(policy IS NULL OR policy = '' OR policy = 'null')"
	const hasUses = "(uses IS NOT NULL AND uses NOT IN ('', 'null', '[]'))"
	if migrator.HasTable(&model.SecretGrant{}) {
		var grants []map[string]any
		if err := db.Table("secret_grants").Select("id", "project_id", "secret_id", "scope", "scope_key", "purpose", "uses").
			Where(noPolicy+" AND ("+hasUses+" OR purpose = ?)", model.SecretGrantPurposeDelegate).
			Order("id").Find(&grants).Error; err != nil {
			return err
		}
		for _, grant := range grants {
			log.Printf("deleted %s grant %s of project %s (secret %s, %s %s, uses %q): a grant an agent takes now carries a policy, and is asked for anew with one",
				columnText(grant["purpose"]), columnText(grant["id"]), columnText(grant["project_id"]), columnText(grant["secret_id"]),
				columnText(grant["scope"]), columnText(grant["scope_key"]), columnText(grant["uses"]))
			if err := db.Exec("DELETE FROM secret_grants WHERE id = ?", grant["id"]).Error; err != nil {
				return err
			}
		}
	}
	if migrator.HasTable(&model.SecretRequest{}) {
		var requests []map[string]any
		if err := db.Table("secret_requests").Select("id", "project_id", "sandbox_id", "uses").
			Where(noPolicy+" AND "+hasUses+" AND status = ?", model.SecretRequestStatusPending).
			Order("id").Find(&requests).Error; err != nil {
			return err
		}
		for _, req := range requests {
			log.Printf("deleted pending request %s of project %s from %s (uses %q): a request now carries a policy, and is asked anew with one",
				columnText(req["id"]), columnText(req["project_id"]), columnText(req["sandbox_id"]), columnText(req["uses"]))
			if err := db.Exec("DELETE FROM secret_requests WHERE id = ?", req["id"]).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// columnText is a text column read into a map, as a driver hands it back,
// or nothing for one that was not selected.
func columnText(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case []byte:
		return string(v)
	default:
		return fmt.Sprint(v)
	}
}

// addsImageKindColumn reports whether the pools table predates image kinds
// (ADR 26-10-09-106 §4): it exists, and has no image_kind column for
// AutoMigrate to add.
func addsImageKindColumn(db *gorm.DB, model any) bool {
	migrator := db.Migrator()
	return migrator.HasTable(model) && !migrator.HasColumn(model, "image_kind")
}

// unsetManifestFileImageKinds takes back the OCI that adding the column gave a
// harness config named by a file:// manifest reference (ADR 0145 §3). Every
// other harness from before kinds is an image, and OCI; a manifest file is
// not, and one from before kinds declares no kind to say which driver's it
// is. Its kind is left undeclared, which places it on no pool, until its file
// declares one and the harness's image is refreshed — a refresh of the file
// as it stands is refused for naming none.
//
// It runs on every start rather than only the one that adds the column, so a
// start that dies between the two cannot leave such a harness OCI for good:
// nothing else would ever correct it. Repeating it touches nothing else,
// because a manifest file registered since kinds always has a VM kind.
func unsetManifestFileImageKinds(db *gorm.DB) error {
	return db.Model(&model.HarnessConfig{}).Where("image LIKE ? AND image_kind = ?", "file://%", platform.OCI).
		UpdateColumn("image_kind", "").Error
}

// respellDiscoVMImageKinds rewrites every pool's and harness's image kind
// stored as `discovm/<driver>`, the spelling releases before ADR 26-10-15-629
// wrote, as `vm/<driver>`, the only spelling platform.ParseImageKind reads.
// It runs on every start, before anything scans a kind; after the first it
// matches nothing.
func respellDiscoVMImageKinds(db *gorm.DB) error {
	const old = "discovm/"
	respelled := gorm.Expr("? || SUBSTR(image_kind, ?)", platform.FormatVM+"/", len(old)+1)
	for _, table := range []any{&model.Pool{}, &model.HarnessConfig{}} {
		if err := db.Model(table).Where("image_kind LIKE ?", old+"%").
			UpdateColumn("image_kind", respelled).Error; err != nil {
			return err
		}
	}
	return nil
}

// backfillPoolImageKinds records OCI for every pool that existed before image
// kinds, which is the kind every one of them runs: there was no other runtime.
//
// It runs only on the start that adds the column, never again. The column's
// default is empty rather than OCI because a pool created since is not known
// to run OCI images until its agent says what it runs — a vm pool runs a
// driver's VM images — and an idempotent rewrite of every empty kind
// would claim OCI for such a pool on every start until its agent reported. A
// start that dies between AutoMigrate and here leaves some old pools empty;
// each is given OCI by its agent's next report, as an agent from before kinds
// declares none (store.recordPoolImageKind).
func backfillPoolImageKinds(db *gorm.DB) error {
	return db.Model(&model.Pool{}).Where("image_kind = ''").
		UpdateColumn("image_kind", platform.OCI).Error
}

// denyOrphanedSecretRequests closes as denied the open requests of sandboxes
// that no longer exist, as deleting a sandbox now does
// (store.denySandboxSecretRequestsTx). Before that, every deleted discobox
// left its asks waiting in the inbox for an agent that was gone. Refresh
// requests are left open: they are about the secret, not the sandbox. It is a
// backfill; after the first run this matches nothing.
func denyOrphanedSecretRequests(db *gorm.DB) error {
	if !db.Migrator().HasTable(&model.SecretRequest{}) {
		return nil
	}
	return db.Model(&model.SecretRequest{}).
		Where("status = ? AND reason <> ? AND sandbox_id <> '' AND sandbox_id NOT IN (?)",
			model.SecretRequestStatusPending, model.SecretRequestReasonRefresh, db.Model(&model.Sandbox{}).Select("id")).
		Updates(map[string]any{"status": model.SecretRequestStatusDenied, "closed_at": time.Now().UTC()}).Error
}

// deleteOrphanedSecretGrants removes the grants scoped to a sandbox or harness
// config that no longer exists. Deleting either now takes its grants with it
// (store.deleteSecretGrantsByScopeTx); before that, every deleted discobox
// left its grants behind, naming an ID that is never reused and so covering
// nothing, while every listing still showed them as standing. It is a
// backfill: store.CreateSecretGrant refuses a grant for a scope that does not
// exist, so after the first run this matches nothing.
func deleteOrphanedSecretGrants(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasTable(&model.SecretGrant{}) {
		return nil
	}
	if err := db.Where("scope = ? AND scope_key NOT IN (?)",
		model.SecretGrantScopeSandbox, db.Model(&model.Sandbox{}).Select("id")).
		Delete(&model.SecretGrant{}).Error; err != nil {
		return err
	}
	return db.Where("scope = ? AND scope_key NOT IN (?)",
		model.SecretGrantScopeHarnessConfig, db.Model(&model.HarnessConfig{}).Select("id")).
		Delete(&model.SecretGrant{}).Error
}

// backfillSecretValueUpdatedAt gives every secret written before
// value_updated_at existed the time its value was last written, which for
// those rows is the last time the row was written at all (ADR 26-09-25-122 §1).
// A value's lifetime runs from it, and updated_at moves on every rename and
// limit change, so a lifetime read from updated_at would restart on each.
// Idempotent: only rows without one are touched.
//
// It is copied through Go rather than in SQL, and written in UTC: SQLite
// compares times as text, updated_at on older rows carries the server's local
// offset, and value_updated_at is compared against UTC times in SQL
// (MarkSecretValueStale), as normalizeCredentialVerdictTimes found for verdicts.
func backfillSecretValueUpdatedAt(db *gorm.DB) error {
	var rows []struct {
		ID        string
		UpdatedAt time.Time
	}
	if err := db.Model(&model.Secret{}).
		Select("id", "updated_at").
		Where("value_updated_at IS NULL").
		Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		if err := db.Model(&model.Secret{}).
			Where("id = ?", row.ID).
			UpdateColumn("value_updated_at", row.UpdatedAt.UTC()).Error; err != nil {
			return err
		}
	}
	return nil
}

// normalizeCredentialVerdictTimes rewrites into UTC any credential verdict
// created_at written before CredentialVerdict stamped it in UTC itself.
//
// Until then GORM's autoCreateTime wrote it in the server's local zone. SQLite
// keeps a time as text carrying that offset and orders and compares it as text,
// so on a server outside UTC every such row sorts, and answers a `since` bound,
// as if it were off by its offset. A rewritten row is the same instant, so this
// changes no row's meaning, only whether a text comparison can read it.
//
// SQLite only: Postgres stores timestamptz and compares instants. The rows to
// rewrite are chosen in SQL by the offset their text ends in, because the
// migration runs on every start and the table only grows: after the first run
// this matches nothing and reads nothing, rather than loading every verdict to
// find it has no work.
func normalizeCredentialVerdictTimes(db *gorm.DB) error {
	if db.Name() != "sqlite" {
		return nil
	}
	var rows []struct {
		ID        string
		CreatedAt time.Time
	}
	if err := db.Model(&model.CredentialVerdict{}).
		Select("id", "created_at").
		Where("created_at NOT LIKE ?", "%+00:00").
		Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		if _, offset := row.CreatedAt.Zone(); offset == 0 {
			continue
		}
		if err := db.Model(&model.CredentialVerdict{}).
			Where("id = ?", row.ID).
			UpdateColumn("created_at", row.CreatedAt.UTC()).Error; err != nil {
			return err
		}
	}
	return nil
}

// migrateCredentialVerdictNeeds rewrites the ask a request verdict recorded
// in the form it now takes (ADR 26-09-26-240 §2). A judge used to ask for a
// body "as text" or "as JSON", and a verdict stored that ask as
// {"body":"text"} or {"body":"json"}; it now asks for the body, as
// {"body":true}, and a stored string would not read as one — failing not the
// row but every listing it is in. What the old ask said beyond wanting the
// body was which form, and the body is no longer shown in forms, so nothing
// the row meant is lost.
//
// The column holds JSON as encoding/json writes it, with no spaces, so the
// rewrite is a string replacement both SQLite and Postgres run the same way.
// The rows are chosen by what they still say, so after the first run this
// matches nothing.
func migrateCredentialVerdictNeeds(db *gorm.DB) error {
	if !db.Migrator().HasTable(&model.CredentialVerdict{}) {
		return nil
	}
	return db.Exec(`UPDATE credential_verdicts
		SET need = REPLACE(REPLACE(need, '"body":"text"', '"body":true'), '"body":"json"', '"body":true')
		WHERE need LIKE '%"body":"text"%' OR need LIKE '%"body":"json"%'`).Error
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
// It runs on every startup, after AutoMigrate and before the API is served,
// for a reason the enum makes sharp: the API validates it on the way out as
// well as in, so a single "git" row left behind would fail to serialize and
// take the whole secret listing with it.
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
// secrets.
//
// Matching itself normalizes both sides (hostscope.Covers, in Go), but the
// host is part of the secret uniqueness index, so a row written with any
// other casing escapes that index. New writes are normalized by the secrets
// service; this repairs the rows written before it was. Requests' and grants'
// hosts are normalized as they move into their lists (moveSecretHostsToLists).
//
// It is a permanent, idempotent step rather than a one-shot: the WHERE clause
// matches nothing once the data is clean, and it costs one indexless scan of
// a small table at startup.
func normalizeSecretHosts(db *gorm.DB) error {
	if !db.Migrator().HasTable("secrets") {
		return nil
	}
	return db.Exec("UPDATE secrets SET host = lower(host) WHERE host <> lower(host)").Error
}

// moveSecretHostsToLists moves the one host a secret request or grant carried
// into the list of hosts that replaced it (ADR 26-10-02-393 §6), then drops
// the retired column.
//
// A row with a host gets that host, normalized, as its only one; a row without
// one — a wildcard grant, a request that named none — keeps an empty list,
// which means the same. Only a row whose list is still unset is written, so a
// step interrupted before the drop and run again changes nothing it already
// moved, and once the column is gone the step does nothing at all.
func moveSecretHostsToLists(db *gorm.DB) error {
	for _, retired := range []struct {
		model any
		table string
	}{
		{&model.SecretRequest{}, "secret_requests"},
		{&model.SecretGrant{}, "secret_grants"},
	} {
		migrator := db.Migrator()
		if !migrator.HasTable(retired.table) || !migrator.HasColumn(retired.model, "host") {
			continue
		}
		var rows []struct {
			ID   string
			Host string
		}
		if err := db.Table(retired.table).Select("id", "host").
			Where("host <> '' AND (hosts IS NULL OR hosts = '' OR hosts = 'null')").
			Scan(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			hosts, err := json.Marshal(hostscope.List(row.Host))
			if err != nil {
				return err
			}
			if err := db.Table(retired.table).Where("id = ?", row.ID).Update("hosts", string(hosts)).Error; err != nil {
				return err
			}
		}
		if err := dropRetiredColumn(db, retired.model, retired.table, "host"); err != nil {
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

// retrustConfiguredCodexWorkingDir moves the trust stanza in a codex harness's
// captured config.toml from the primary source's target to `workingDir`, the
// directory the sandbox's terminals start in.
//
// The configure flow returns config.toml as a configured file, and a configured
// file overlays the image's by path. A config configured before trust followed
// the working directory therefore kept the stanza that ranges over `sources`,
// whatever image it ran: a sandbox with no source rendered no trust, and codex
// opened on its trust screen for the workspace it was already sitting in, until
// somebody reconfigured the harness.
//
// Only the exact stanza that configure flow wrote is replaced, so a file somebody
// has edited around it is still fixed and one they rewrote is left as they left
// it. It runs on every start. The stanza it writes carries the old one as its
// fallback, so a file already holding it is skipped rather than matched again;
// once every file is, it writes nothing. updated_at is left alone: a migration is
// not an edit anybody made.
func retrustConfiguredCodexWorkingDir(db *gorm.DB) error {
	if !db.Migrator().HasTable(&model.HarnessConfig{}) {
		return nil
	}
	var configs []model.HarnessConfig
	if err := db.Select("id", "configured_files").Find(&configs).Error; err != nil {
		return err
	}
	for _, config := range configs {
		changed := false
		for i, file := range config.ConfiguredFiles {
			if file.Path != codexConfigPath || !file.Template ||
				!strings.Contains(file.Content, codexSourceTrustStanza) ||
				strings.Contains(file.Content, codexWorkingDirTrustStanza) {
				continue
			}
			config.ConfiguredFiles[i].Content = strings.ReplaceAll(file.Content, codexSourceTrustStanza, codexWorkingDirTrustStanza)
			changed = true
		}
		if !changed {
			continue
		}
		if err := db.Model(&model.HarnessConfig{ID: config.ID}).
			Select("configured_files").
			UpdateColumns(model.HarnessConfig{ConfiguredFiles: config.ConfiguredFiles}).Error; err != nil {
			return fmt.Errorf("retrust codex working directory for harness config %s: %w", config.ID, err)
		}
	}
	return nil
}

// codexConfigPath is where the codex harness's configure flow captures its
// settings, relative to the harness's home.
const codexConfigPath = ".codex/config.toml"

// codexSourceTrustStanza is the stanza harness/codex-cli/configure.sh appended to
// a captured config.toml before trust followed the working directory. It is only
// ever read as the evidence of a file written then.
const codexSourceTrustStanza = "{{- range .sources }}{{- if eq .slug \"primary\" }}\n[projects.{{ .target | json }}]\ntrust_level = \"trusted\"\n{{- end }}{{- end }}"

// codexWorkingDirTrustStanza is what replaces it: trust `workingDir`, and where
// the agent predates that key, the primary source's target as the old stanza did.
//
// It is not the stanza configure.sh appends now, which trusts nothing on such an
// agent. That script's output comes back from the image that sets the key; this
// one lands in every stored config whatever image it names, and a config on an
// older image must not lose the trust it had.
const codexWorkingDirTrustStanza = "{{- if .workingDir }}\n[projects.{{ .workingDir | json }}]\ntrust_level = \"trusted\"\n{{- else }}" + codexSourceTrustStanza + "{{- end }}"

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
// sandboxes no longer reserve a slice of their pool's CPU, memory, or storage,
// so nothing writes these columns anymore. The same-named columns on pools are
// the pool's own size and are untouched.
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

// rekeySandboxOrigins files every sandbox under the origin key ADR 0111
// derives: its origin host and its primary source's root, or the host alone
// when it has no source. A local-sourced sandbox's key does not change; a
// remote-sourced one moves from the directory it was created in to its URL,
// and a sourceless one to its host.
//
// It reads only the host and the source, so a row's stored origin keeps the
// project path it was created with: the model no longer declares the field,
// and decoding ignores it. The key is written with UpdateColumn, which leaves
// updated_at alone — listings order by it, and a migration is not an update
// anybody made. It runs on every start and writes only the rows whose key
// differs.
func rekeySandboxOrigins(db *gorm.DB) error {
	var rows []model.Sandbox
	if err := db.Select("id", "origin", "source", "origin_key").
		Where("origin IS NOT NULL").
		Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		key := model.SandboxOriginKey(row.Origin, row.Source)
		if key == "" || (row.OriginKey != nil && *row.OriginKey == key) {
			continue
		}
		if err := db.Model(&model.Sandbox{}).Where("id = ?", row.ID).UpdateColumn("origin_key", key).Error; err != nil {
			return err
		}
	}
	return nil
}

// renameSandboxEgressReconcile moves the pending pushes of a sandbox's egress
// policy to the reconcile that now pushes its whole proxy policy, egress and
// gateways together (ADR 26-10-16-470 §5). A mark carries no intent of its
// own, only that the push is owed, so a sandbox marked under both names keeps
// the new mark and drops the old one.
func renameSandboxEgressReconcile(db *gorm.DB) error {
	if !db.Migrator().HasTable("reconcile_dirty") {
		return nil
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`UPDATE reconcile_dirty SET resource_type = ?
			WHERE resource_type = ? AND resource_id NOT IN (
				SELECT resource_id FROM reconcile_dirty WHERE resource_type = ?)`,
			"sandboxProxyPolicy", "sandboxEgress", "sandboxProxyPolicy").Error; err != nil {
			return err
		}
		return tx.Exec("DELETE FROM reconcile_dirty WHERE resource_type = ?", "sandboxEgress").Error
	})
}

// dropPoolImageStaging retires the prepull condition and its queued work while
// preserving pool identities, desired state, and all runtime data.
func dropPoolImageStaging(db *gorm.DB) error {
	for _, column := range []string{"images_staged", "image_stage", "image_staged_at"} {
		if err := dropRetiredColumn(db, &model.Pool{}, "pools", column); err != nil {
			return err
		}
	}
	if db.Migrator().HasTable("reconcile_dirty") {
		return db.Exec("DELETE FROM reconcile_dirty WHERE resource_type = ?", "poolImages").Error
	}
	return nil
}
