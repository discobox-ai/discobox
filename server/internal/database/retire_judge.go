package database

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/x/id"
)

// gateWellKnownID is the well-known ID the discobox API's gate secret was
// marked with (ADR 0140 §1). Calling the API is a sandbox flag now
// (ADR 26-10-27-880 §9), and nothing else names it.
const gateWellKnownID = "ai.discobox.sandbox"

// retireCredentialJudge takes out of the database everything a judge enforced
// or recorded (ADR 26-10-27-880 §8), without widening what any discobox may
// send:
//
//   - a secret's or a harness config's route whose use a judge read becomes a
//     blocked route, so the requests it covered are refused rather than sent
//     unjudged, and the sentinel delivery of every discobox holding it is
//     marked in the same transaction, so its pool is sent the blocked route
//     promptly rather than at the next unrelated push;
//   - every grant that carried uses or a policy — what only a judge enforced,
//     delegations included — is taken back. One held by a sandbox becomes a
//     pending request from it for a person to answer; a wider one is deleted.
//     Its agent binding goes with it, all in one transaction per grant;
//   - every live host trust approved with uses is deleted: its uses were the
//     only limit on what was sent to the pinned host, and the agent asks
//     again for a pin alone;
//   - the discobox API's gate secret goes with its grants and bindings. No
//     sandbox is given the flag that replaces it: a person sets it again;
//   - each project's judge discobox is marked for deletion, and no longer
//     reads as a judge, so no row the API serves carries the retired mode;
//   - the verdict and approval rule tables are dropped, and the columns only
//     the judge read with them, and so is the queued work of the reconcile
//     types that went with them.
//
// Every part is guarded by what it is looking for, so it runs on every start
// and does nothing once there is nothing left. A start interrupted part way
// finds the rest and finishes: each conversion commits with what it converts
// away, the drops come last, and the columns they drop are what says there is
// still work.
func retireCredentialJudge(db *gorm.DB) error {
	if err := blockJudgedRoutes(db, "secrets", "routes", blockRoutes, secretHolders); err != nil {
		return err
	}
	if err := blockJudgedRoutes(db, "harness_configs", "secrets", blockHarnessSecretRoutes, harnessConfigHolders); err != nil {
		return err
	}
	if err := retireJudgeSandboxes(db); err != nil {
		return err
	}
	if err := retireJudgedHostTrusts(db); err != nil {
		return err
	}
	if err := retireGateSecrets(db); err != nil {
		return err
	}
	if err := retireJudgedGrants(db); err != nil {
		return err
	}
	if err := retireAgentBindings(db); err != nil {
		return err
	}
	return dropJudgeSchema(db)
}

// blockJudgedRoutes rewrites one JSON column of a table through convert,
// writing back only the rows convert changed, each with the sentinel delivery
// of the discoboxes holders names marked in the same transaction. Rows are
// chosen by a "use" key in the text, which every route a judge read carried,
// so after the first run nothing is read.
func blockJudgedRoutes(db *gorm.DB, table, column string, convert func(string) (string, bool, error), holders func(*gorm.DB, string) ([]sandboxRef, error)) error {
	migrator := db.Migrator()
	if !migrator.HasTable(table) || !migrator.HasColumn(table, column) {
		return nil
	}
	var rows []map[string]any
	if err := db.Table(table).Select("id", column).Where(column+" LIKE ?", `%"use"%`).Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		next, changed, err := convert(columnText(row[column]))
		if err != nil {
			return fmt.Errorf("%s %s: %w", table, columnText(row["id"]), err)
		}
		if !changed {
			continue
		}
		log.Printf("blocked the judged routes of %s %s: a route a judge read refuses its requests now, rather than sending them unjudged (ADR 26-10-27-880)",
			strings.TrimSuffix(table, "s"), columnText(row["id"]))
		if err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Table(table).Where("id = ?", row["id"]).UpdateColumn(column, next).Error; err != nil {
				return err
			}
			held, err := holders(tx, columnText(row["id"]))
			if err != nil {
				return err
			}
			return markDirty(tx, sentinelsResourceType, held)
		}); err != nil {
			return err
		}
	}
	return nil
}

// sandboxRef names one sandbox the way its reconcile marks do.
type sandboxRef struct {
	ProjectID string
	ID        string
}

// secretHolders are the discoboxes a secret is bound in: the ones whose pool
// holds its routes.
func secretHolders(db *gorm.DB, secretID string) ([]sandboxRef, error) {
	var out []sandboxRef
	err := db.Table("sandbox_secrets").Select("DISTINCT project_id, sandbox_id AS id").
		Where("secret_id = ?", secretID).Scan(&out).Error
	return out, err
}

// harnessConfigHolders are the discoboxes running a harness config, whose
// credentials' routes it declared.
func harnessConfigHolders(db *gorm.DB, configID string) ([]sandboxRef, error) {
	var out []sandboxRef
	err := db.Table("sandboxes").Select("project_id, id").
		Where("harness_config_id = ? AND desired_state <> ?", configID, model.DesiredStateDeleted).Scan(&out).Error
	return out, err
}

// The reconcile types this step marks or retires, named here rather than
// imported: the database package sits below the resources that own them.
const (
	sandboxResourceType   = "sandbox"
	sentinelsResourceType = "sandboxSentinels"
)

// retiredReconcileTypes are the reconcile types that went with the judge:
// the project's judge convergence and the approval rule tests.
var retiredReconcileTypes = []string{"judge", "ruleRequest"}

// markDirty marks each sandbox for one reconcile type with the engine's own
// mark (reconcile.MarkTx), in the caller's transaction. The table is the
// engine's; a database the engine has never run against has no marks to make.
func markDirty(db *gorm.DB, resourceType string, sandboxes []sandboxRef) error {
	if len(sandboxes) == 0 || !db.Migrator().HasTable("reconcile_dirty") {
		return nil
	}
	for _, sandbox := range sandboxes {
		if err := reconcile.MarkTx(context.Background(), db, resourceType, sandboxDirtyID(sandbox)); err != nil {
			return err
		}
	}
	return nil
}

// sandboxDirtyID is the ID a sandbox's reconciles are marked under,
// sandboxes.SandboxDirtyID's spelling, which this package sits below.
func sandboxDirtyID(sandbox sandboxRef) string { return sandbox.ProjectID + "/" + sandbox.ID }

// retiredJudgeHarnessMode is the harness mode a project's judge discobox ran
// in (ADR 26-09-22-838 §1), which the API no longer names.
const retiredJudgeHarnessMode = "judge"

// retireJudgeSandboxes marks every judge discobox for deletion, as recording
// the intent through the API does — the delete desired, a new generation, and
// its reconcile marked — and takes the retired mode off it in the same write:
// the API's harness mode is run or config, and a row the API serves while its
// purge waits on a pool that may never answer must read as one of them.
func retireJudgeSandboxes(db *gorm.DB) error {
	if !db.Migrator().HasTable("sandboxes") {
		return nil
	}
	var judges []sandboxRef
	if err := db.Table("sandboxes").Select("project_id, id").Where("harness_mode = ?", retiredJudgeHarnessMode).Scan(&judges).Error; err != nil {
		return err
	}
	for _, judge := range judges {
		log.Printf("removing judge discobox %s of project %s: nothing judges credential use any more (ADR 26-10-27-880)", judge.ID, judge.ProjectID)
		if err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Table("sandboxes").Where("id = ?", judge.ID).UpdateColumns(map[string]any{
				"harness_mode":  sandboxconfig.HarnessModeRun,
				"desired_state": model.DesiredStateDeleted,
				"generation":    gorm.Expr("generation + 1"),
				"error_message": nil,
				"error_reason":  "",
			}).Error; err != nil {
				return err
			}
			return markDirty(tx, sandboxResourceType, []sandboxRef{judge})
		}); err != nil {
			return err
		}
	}
	return nil
}

// retireJudgedHostTrusts deletes every live host trust approved with uses
// (ADR 0149 §5): the proxy judged every request to the pinned host against
// them, and without a judge the pin would admit them all. A lapsed one admits
// nothing and is left. The agent asks again with `discobox-access trust`, and
// a person pins the host alone.
func retireJudgedHostTrusts(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasTable("host_trusts") || !migrator.HasColumn("host_trusts", "uses") {
		return nil
	}
	var trusts []map[string]any
	if err := db.Table("host_trusts").Select("id", "project_id", "sandbox_id", "host", "uses").
		Where("uses IS NOT NULL AND uses NOT IN ('', 'null', '[]') AND expires_at > ?", time.Now().UTC()).
		Order("id").Find(&trusts).Error; err != nil {
		return err
	}
	for _, trust := range trusts {
		log.Printf("deleted host trust %s of sandbox %s in project %s (host %s, uses %q): a judge held what was sent there to its uses, and none does now; the sandbox asks again for a pin alone (ADR 26-10-27-880 §10)",
			columnText(trust["id"]), columnText(trust["sandbox_id"]), columnText(trust["project_id"]), columnText(trust["host"]), columnText(trust["uses"]))
		if err := db.Exec("DELETE FROM host_trusts WHERE id = ?", trust["id"]).Error; err != nil {
			return err
		}
	}
	return nil
}

// blockRoutes rewrites a secret's routes: one that carried a use is blocked,
// and no route keeps a use or a use ID.
func blockRoutes(text string) (string, bool, error) {
	var routes []map[string]any
	if err := json.Unmarshal([]byte(text), &routes); err != nil {
		return "", false, err
	}
	changed := blockRouteList(routes)
	if !changed {
		return text, false, nil
	}
	out, err := json.Marshal(routes)
	return string(out), true, err
}

// blockHarnessSecretRoutes does the same to the routes each secret a harness
// config declares carries.
func blockHarnessSecretRoutes(text string) (string, bool, error) {
	var secrets []map[string]any
	if err := json.Unmarshal([]byte(text), &secrets); err != nil {
		return "", false, err
	}
	changed := false
	for _, secret := range secrets {
		raw, ok := secret["routes"].([]any)
		if !ok {
			continue
		}
		routes := make([]map[string]any, 0, len(raw))
		for _, route := range raw {
			if m, ok := route.(map[string]any); ok {
				routes = append(routes, m)
			}
		}
		if blockRouteList(routes) {
			changed = true
		}
	}
	if !changed {
		return text, false, nil
	}
	out, err := json.Marshal(secrets)
	return string(out), true, err
}

func blockRouteList(routes []map[string]any) bool {
	changed := false
	for _, route := range routes {
		use, hasUse := route["use"]
		_, hasUseID := route["useId"]
		if !hasUse && !hasUseID {
			continue
		}
		if text, _ := use.(string); strings.TrimSpace(text) != "" {
			route["blocked"] = true
		}
		delete(route, "use")
		delete(route, "useId")
		changed = true
	}
	return changed
}

// retireGateSecrets deletes the discobox API's gate secret of every project,
// with everything that names it: its grants, the sandboxes' bindings of it,
// and the requests for it still open. The secret had no value worth keeping:
// it stood for no credential (ADR 0140 §1).
func retireGateSecrets(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasTable("secrets") || !migrator.HasColumn("secrets", "well_known_id") {
		return nil
	}
	var gates []map[string]any
	if err := db.Table("secrets").Select("id", "project_id").Where("well_known_id = ?", gateWellKnownID).Find(&gates).Error; err != nil {
		return err
	}
	for _, gate := range gates {
		secretID := columnText(gate["id"])
		log.Printf("deleted the discobox API gate secret %s of project %s, with its grants and bindings: calling the API is a sandbox's discoboxApi flag now, which a person sets (ADR 26-10-27-880 §9)",
			secretID, columnText(gate["project_id"]))
		for _, stmt := range []string{
			"DELETE FROM secret_grants WHERE secret_id = ?",
			"DELETE FROM sandbox_secrets WHERE secret_id = ?",
			"DELETE FROM harness_config_secret_bindings WHERE secret_id = ?",
			"DELETE FROM secret_rejections WHERE secret_id = ?",
			"DELETE FROM secrets WHERE id = ?",
		} {
			if err := db.Exec(stmt, secretID).Error; err != nil {
				return err
			}
		}
	}
	if migrator.HasColumn("secret_requests", "well_known_id") {
		if err := db.Exec("DELETE FROM secret_requests WHERE well_known_id = ? AND status = ?",
			gateWellKnownID, model.SecretRequestStatusPending).Error; err != nil {
			return err
		}
	}
	return nil
}

// retireJudgedGrants takes back every grant a judge enforced: one with uses or
// a policy, delegations among them. A grant held by one sandbox is asked for
// again, as a pending request from that sandbox naming the secret, variable,
// hosts and purpose it had, so a person answers it under the static rules;
// it never becomes a live grant on its own. A wider one names no sandbox to
// ask for, and is deleted. Each is logged with what it said.
func retireJudgedGrants(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasTable("secret_grants") {
		return nil
	}
	hasUses := migrator.HasColumn("secret_grants", "uses")
	hasPolicy := migrator.HasColumn("secret_grants", "policy")
	if !hasUses && !hasPolicy {
		return nil
	}
	var judged []string
	columns := []string{"id", "project_id", "secret_id", "scope", "scope_key", "purpose", "hosts"}
	if hasUses {
		judged = append(judged, "(uses IS NOT NULL AND uses NOT IN ('', 'null', '[]'))")
		columns = append(columns, "uses")
	}
	if hasPolicy {
		judged = append(judged, "(policy IS NOT NULL AND policy NOT IN ('', 'null'))")
	}
	hasEnv := migrator.HasColumn("secret_grants", "env_name")
	if hasEnv {
		columns = append(columns, "env_name")
	}
	var grants []map[string]any
	if err := db.Table("secret_grants").Select(columns).Where(strings.Join(judged, " OR ")).Order("id").Find(&grants).Error; err != nil {
		return err
	}
	hasAgentBindings := migrator.HasColumn("sandbox_secrets", "agent_requested")
	for _, grant := range grants {
		if err := db.Transaction(func(tx *gorm.DB) error {
			return retireJudgedGrant(tx, grant, hasAgentBindings)
		}); err != nil {
			return err
		}
	}
	return nil
}

// retireJudgedGrant takes back one judged grant: the request it is asked
// again as, the agent binding it delivered, and the grant itself, in the
// caller's transaction, so a start interrupted part way leaves the grant to
// be taken back whole on the next rather than asked again twice.
func retireJudgedGrant(db *gorm.DB, grant map[string]any, hasAgentBindings bool) error {
	grantID := columnText(grant["id"])
	projectID := columnText(grant["project_id"])
	secretID := columnText(grant["secret_id"])
	scope := columnText(grant["scope"])
	sandboxID := columnText(grant["scope_key"])
	envName := columnText(grant["env_name"])
	if scope == model.SecretGrantScopeSandbox {
		if hasAgentBindings && envName == "" {
			var bound []map[string]any
			if err := db.Table("sandbox_secrets").Select("env_name").
				Where("sandbox_id = ? AND secret_id = ? AND agent_requested = ?", sandboxID, secretID, true).
				Limit(1).Find(&bound).Error; err != nil {
				return err
			}
			if len(bound) == 1 {
				envName = columnText(bound[0]["env_name"])
			}
		}
		opened, err := reaskForGrant(db, grant, envName)
		if err != nil {
			return err
		}
		if opened != "" {
			log.Printf("took back %s grant %s of project %s (secret %s, sandbox %s, uses %q): a judge enforced it, and none does now; asked again as pending request %s for a person to answer (ADR 26-10-27-880 §8)",
				columnText(grant["purpose"]), grantID, projectID, secretID, sandboxID, columnText(grant["uses"]), opened)
		} else {
			log.Printf("took back %s grant %s of project %s (secret %s, sandbox %s, uses %q): a judge enforced it, and none does now; its sandbox is gone, so nothing is asked again (ADR 26-10-27-880 §8)",
				columnText(grant["purpose"]), grantID, projectID, secretID, sandboxID, columnText(grant["uses"]))
		}
		if hasAgentBindings {
			if err := db.Exec("DELETE FROM sandbox_secrets WHERE sandbox_id = ? AND secret_id = ? AND agent_requested = ?", sandboxID, secretID, true).Error; err != nil {
				return err
			}
		}
	} else {
		log.Printf("took back %s grant %s of project %s (secret %s, %s %s, uses %q): a judge enforced it, and none does now; it names no one sandbox to ask again for (ADR 26-10-27-880 §8)",
			columnText(grant["purpose"]), grantID, projectID, secretID, scope, sandboxID, columnText(grant["uses"]))
	}
	if err := db.Exec("DELETE FROM secret_grants WHERE id = ?", grantID).Error; err != nil {
		return err
	}
	return nil
}

// reaskForGrant opens the pending request a taken-back sandbox grant becomes,
// and returns its ID; it opens none for a sandbox that no longer exists.
func reaskForGrant(db *gorm.DB, grant map[string]any, envName string) (string, error) {
	sandboxID := columnText(grant["scope_key"])
	var sandboxes int64
	if err := db.Table("sandboxes").Where("id = ?", sandboxID).Count(&sandboxes).Error; err != nil {
		return "", err
	}
	if sandboxes == 0 {
		return "", nil
	}
	var secrets []map[string]any
	if err := db.Table("secrets").Select("name", "type").Where("id = ?", columnText(grant["secret_id"])).Limit(1).Find(&secrets).Error; err != nil {
		return "", err
	}
	if len(secrets) == 0 {
		return "", nil
	}
	purpose := columnText(grant["purpose"])
	if purpose == "" {
		purpose = model.SecretGrantPurposeUse
	}
	requestID, err := id.New(id.PrefixSecretRequest)
	if err != nil {
		return "", err
	}
	justification := "Asked again for the grant this discobox held, which a judge enforced: the judge is gone, so a person decides (ADR 26-10-27-880)."
	if uses := columnText(grant["uses"]); uses != "" && uses != "null" {
		justification += " It was granted for: " + uses
	}
	// Hosts are copied as the grant stored them, a JSON list; a grant with
	// none asks for none.
	var hosts any
	if text := columnText(grant["hosts"]); text != "" {
		hosts = text
	}
	now := time.Now().UTC()
	err = db.Exec(`INSERT INTO secret_requests
		(id, project_id, requested_by, sandbox_id, type, hosts, name, env_name, justification, purpose, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		requestID, columnText(grant["project_id"]), "agent:"+sandboxID, sandboxID, columnText(secrets[0]["type"]),
		hosts, columnText(secrets[0]["name"]), envName, justification, purpose,
		model.SecretRequestStatusPending, now, now).Error
	return requestID, err
}

// retireAgentBindings deletes the agent bindings no grant took with it — ones
// whose grant had already gone — and then the column that marked them, with
// the uniqueness index it was part of, which AutoMigrate recreated over the
// sandbox and variable alone when it is missing. An agent binding's sentinel
// never reached its sandbox, so nothing in one is left holding it.
func retireAgentBindings(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasTable(&model.SandboxSecret{}) || !migrator.HasColumn(&model.SandboxSecret{}, "agent_requested") {
		return nil
	}
	if err := db.Exec("DELETE FROM sandbox_secrets WHERE agent_requested = ?", true).Error; err != nil {
		return err
	}
	if migrator.HasIndex(&model.SandboxSecret{}, sandboxSecretEnvIndex) {
		if err := migrator.DropIndex(&model.SandboxSecret{}, sandboxSecretEnvIndex); err != nil {
			return err
		}
	}
	if err := dropRetiredColumn(db, &model.SandboxSecret{}, "sandbox_secrets", "agent_requested"); err != nil {
		return err
	}
	if migrator.HasIndex(&model.SandboxSecret{}, sandboxSecretEnvIndex) {
		return nil
	}
	return migrator.CreateIndex(&model.SandboxSecret{}, sandboxSecretEnvIndex)
}

// dropJudgeSchema drops what only the judge read: the queued work of the
// reconcile types that went with it, which the engine no longer claims; the
// verdict and approval rule tables; and the columns that held uses,
// policies, rules, well-known IDs, and a project's judge.
func dropJudgeSchema(db *gorm.DB) error {
	migrator := db.Migrator()
	if migrator.HasTable("reconcile_dirty") {
		if err := db.Exec("DELETE FROM reconcile_dirty WHERE resource_type IN ?", retiredReconcileTypes).Error; err != nil {
			return err
		}
	}
	for _, table := range []string{"credential_verdicts", "approval_rules"} {
		if migrator.HasTable(table) {
			if err := migrator.DropTable(table); err != nil {
				return err
			}
		}
	}
	for _, retired := range []struct {
		model   any
		table   string
		columns []string
	}{
		{&model.SecretGrant{}, "secret_grants", []string{"uses", "policy", "rule_id", "env_name"}},
		{&model.SecretRequest{}, "secret_requests", []string{"uses", "policy", "well_known_id"}},
		{&model.Secret{}, "secrets", []string{"well_known_id"}},
		{&model.HostTrust{}, "host_trusts", []string{"uses"}},
		{&model.HostTrustRequest{}, "host_trust_requests", []string{"uses"}},
		{&model.Project{}, "projects", []string{"judge_pool_id", "judge_sandbox_id"}},
		{&model.SecretRejection{}, "secret_rejections", []string{"use_id"}},
	} {
		if !migrator.HasTable(retired.table) {
			continue
		}
		for _, column := range retired.columns {
			if err := dropRetiredColumn(db, retired.model, retired.table, column); err != nil {
				return err
			}
		}
	}
	return nil
}
