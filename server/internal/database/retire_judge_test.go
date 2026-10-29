package database_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/resources/sandboxes"
	"github.com/discobox-ai/x/gormdb"
)

// judgeEraSchema puts back what a database written while a judge enforced
// credential use carried (ADR 26-10-27-880 §8): the columns that held uses,
// policies, rules, well-known IDs and a project's judge, the agent binding
// flag in the variable's uniqueness index, and the verdict and rule tables.
func judgeEraSchema(t *testing.T, db *database.DB) {
	t.Helper()
	for _, stmt := range []string{
		`ALTER TABLE secret_grants ADD COLUMN uses text`,
		`ALTER TABLE secret_grants ADD COLUMN policy text`,
		`ALTER TABLE secret_grants ADD COLUMN rule_id text NOT NULL DEFAULT ''`,
		`ALTER TABLE secret_grants ADD COLUMN env_name text NOT NULL DEFAULT ''`,
		`ALTER TABLE secret_requests ADD COLUMN uses text`,
		`ALTER TABLE secret_requests ADD COLUMN policy text`,
		`ALTER TABLE secret_requests ADD COLUMN well_known_id text NOT NULL DEFAULT ''`,
		`ALTER TABLE secrets ADD COLUMN well_known_id text NOT NULL DEFAULT ''`,
		`CREATE UNIQUE INDEX idx_secret_project_well_known ON secrets (project_id, well_known_id) WHERE well_known_id <> ''`,
		`ALTER TABLE host_trusts ADD COLUMN uses text`,
		`ALTER TABLE host_trust_requests ADD COLUMN uses text`,
		`ALTER TABLE projects ADD COLUMN judge_pool_id text DEFAULT ''`,
		`ALTER TABLE projects ADD COLUMN judge_sandbox_id text DEFAULT ''`,
		`ALTER TABLE secret_rejections ADD COLUMN use_id text NOT NULL DEFAULT ''`,
		`DROP INDEX idx_sandbox_secret_env`,
		`ALTER TABLE sandbox_secrets ADD COLUMN agent_requested numeric NOT NULL DEFAULT false`,
		`CREATE UNIQUE INDEX idx_sandbox_secret_env ON sandbox_secrets (sandbox_id, env_name, agent_requested)`,
		`CREATE TABLE credential_verdicts (id text PRIMARY KEY, project_id text)`,
		`CREATE TABLE approval_rules (id text PRIMARY KEY, project_id text)`,
	} {
		if err := db.Write.Exec(stmt).Error; err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// Upgrading from the judge takes back everything only it enforced and widens
// nothing: a judged route refuses, a judged grant held by a sandbox becomes a
// pending request from it and one wider is deleted, the gate secret goes, and
// so does every column and table the judge read. Standing grants and injected
// bindings are untouched, and a second run changes nothing.
func TestMigrateRetiresTheCredentialJudge(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{
		Driver: gormdb.DriverSQLite,
		DSN:    "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db"),
	})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close database: %v", err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}
	// The engine's table, as a server that ran one has it.
	if _, err := reconcile.New(db.Write, reconcile.Options{}); err != nil {
		t.Fatalf("reconcile table: %v", err)
	}
	grantEraSchema(t, db)
	judgeEraSchema(t, db)
	write := db.Write.WithContext(ctx)
	if err := write.Exec(`INSERT INTO projects (id, owner_user_id, name, judge_sandbox_id) VALUES ('project-1', 'user-1', 'Project', 'sbx-judge')`).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := write.Create(&model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "Docker"}).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if err := write.Create(&model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: "provider-1"}}).Error; err != nil {
		t.Fatalf("create pool: %v", err)
	}
	live, lapsed := time.Now().UTC().Add(time.Hour), time.Now().UTC().Add(-time.Hour)
	for _, retired := range []string{"judge", "ruleRequest"} {
		if err := write.Exec(`INSERT INTO reconcile_dirty (resource_type, resource_id, seq, not_before, attempts, marked_at, created_at, updated_at)
			VALUES (?, 'project-1', 1, ?, 0, ?, ?, ?)`, retired, lapsed, lapsed, lapsed, lapsed).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := write.Create(&model.Sandbox{ID: "sbx-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "one"}).Error; err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO secrets (id, project_id, name, type, max_grant_ttl_seconds, routes) VALUES
			('sec-gh', 'project-1', 'github', 'token', 0, '[{"host":"githubcopilot.com"},{"host":"githubcopilot.com","path":"/mcp","use":"read-only MCP","useId":"use_mcp"},{"host":"github.com","useId":"use_stale"}]')`,
		`INSERT INTO secrets (id, project_id, name, type, max_grant_ttl_seconds, well_known_id) VALUES ('sec-gate', 'project-1', 'discobox', 'token', 0, 'ai.discobox.sandbox')`,
		`INSERT INTO harness_configs (id, project_id, slug, name, secrets) VALUES ('hc-1', 'project-1', 'copilot', 'Copilot',
			'[{"name":"COPILOT_GITHUB_TOKEN","routes":[{"host":"githubcopilot.com"},{"host":"github.com","use":"light reads"}]}]')`,
		// A judged sandbox grant and its agent binding; a judged delegation; a
		// judged project grant; a standing grant; the gate's grant.
		`INSERT INTO secret_grants (id, project_id, secret_id, scope, scope_key, hosts, purpose, uses, policy, env_name) VALUES
			('g-agent', 'project-1', 'sec-gh', 'sandbox', 'sbx-1', '["api.github.com"]', 'use', '[{"useId":"use_1","description":"open a PR"}]', '{"statements":[]}', 'GH_TOKEN'),
			('g-delegate', 'project-1', 'sec-gh', 'sandbox', 'sbx-1', '["github.com"]', 'delegate', '[{"useId":"use_2","description":"hand on"}]', '{"statements":[]}', ''),
			('g-project', 'project-1', 'sec-gh', 'project', 'project-1', '["github.com"]', 'use', '[{"useId":"use_3","description":"anything"}]', NULL, 'GH_TOKEN'),
			('g-standing', 'project-1', 'sec-gh', 'project', 'project-1', '["github.com"]', 'use', NULL, NULL, ''),
			('g-gate', 'project-1', 'sec-gate', 'sandbox', 'sbx-1', '["api.discobox.internal"]', 'use', '[{"useId":"use_4","description":"create boxes"}]', '{"statements":[]}', 'DISCOBOX_TOKEN')`,
		`INSERT INTO sandbox_secrets (id, project_id, sandbox_id, secret_id, env_name, sentinel, agent_requested) VALUES
			('ss-agent', 'project-1', 'sbx-1', 'sec-gh', 'GH_TOKEN', 'SENTINEL-AGENT', true),
			('ss-injected', 'project-1', 'sbx-1', 'sec-gh', 'GH_TOKEN', 'SENTINEL-INJECTED', false),
			('ss-gate', 'project-1', 'sbx-1', 'sec-gate', 'DISCOBOX_TOKEN', 'SENTINEL-GATE', true)`,
		`INSERT INTO credential_verdicts (id, project_id) VALUES ('cv-1', 'project-1')`,
		`INSERT INTO approval_rules (id, project_id) VALUES ('rule-1', 'project-1')`,
	} {
		if err := write.Exec(stmt).Error; err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	hc := "hc-1"
	for _, sandbox := range []*model.Sandbox{
		{ID: "sbx-judge", Name: "judge", SandboxManifest: model.SandboxManifest{HarnessMode: "judge"}},
		{ID: "sbx-copilot", Name: "copilot", SandboxManifest: model.SandboxManifest{HarnessConfigID: &hc}},
	} {
		sandbox.ProjectID, sandbox.PoolID, sandbox.CreatedByUserID = "project-1", "pool-1", "user-1"
		if err := write.Create(sandbox).Error; err != nil {
			t.Fatalf("create sandbox %s: %v", sandbox.ID, err)
		}
	}
	for _, trust := range []*model.HostTrust{
		{ID: "ht-judged", ExpiresAt: live},
		{ID: "ht-lapsed", ExpiresAt: lapsed},
		{ID: "ht-pin", ExpiresAt: live},
	} {
		trust.ProjectID, trust.SandboxID, trust.Host, trust.GrantedBy = "project-1", "sbx-judge", "10.0.0.1:443", "user-1"
		trust.Pin = model.TrustPin{Kind: "leaf-spki", SHA256: "ab"}
		if err := write.Create(trust).Error; err != nil {
			t.Fatalf("create trust %s: %v", trust.ID, err)
		}
	}
	if err := write.Exec(`UPDATE host_trusts SET uses = '[{"useId":"use_k","description":"read pods"}]' WHERE id IN ('ht-judged', 'ht-lapsed')`).Error; err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })

	for run := 1; run <= 2; run++ {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("upgrade migrate %d: %v", run, err)
		}

		var secret model.Secret
		if err := write.First(&secret, "id = ?", "sec-gh").Error; err != nil {
			t.Fatalf("run %d: read secret: %v", run, err)
		}
		wantRoutes := []model.SecretRoute{
			upgraded("githubcopilot.com", "", false),
			upgraded("githubcopilot.com", "/mcp", true),
			// A use ID with no use was never judged, and stays open.
			upgraded("github.com", "", false),
		}
		if !reflect.DeepEqual(secret.Routes, wantRoutes) {
			t.Fatalf("run %d: routes = %+v, want %+v", run, secret.Routes, wantRoutes)
		}
		var config model.HarnessConfig
		if err := write.First(&config, "id = ?", "hc-1").Error; err != nil {
			t.Fatalf("run %d: read harness config: %v", run, err)
		}
		if got := config.Secrets[0].Routes; len(got) != 2 || got[0].Blocked || !got[1].Blocked {
			t.Fatalf("run %d: harness routes = %+v, want the judged one blocked", run, got)
		}

		// The standing grant, the only one left, covered the injected
		// binding, which is bound on its sandbox (ADR 26-10-27-880 §8).
		var bindings []model.SecretBinding
		if err := write.Order("id").Find(&bindings).Error; err != nil {
			t.Fatalf("run %d: list bindings: %v", run, err)
		}
		if len(bindings) != 1 || bindings[0].ID != "bind_ss-injected" || bindings[0].TargetID != "sbx-1" ||
			!reflect.DeepEqual(bindings[0].Routes, []model.SecretRoute{upgraded("github.com", "", false)}) {
			t.Fatalf("run %d: bindings = %+v, want the standing grant's", run, bindings)
		}
		var sentinels []string
		if err := write.Model(&model.SandboxSecret{}).Order("id").Pluck("id", &sentinels).Error; err != nil {
			t.Fatalf("run %d: list sentinels: %v", run, err)
		}
		if want := []string{"ss-injected"}; !reflect.DeepEqual(sentinels, want) {
			t.Fatalf("run %d: sentinels = %v, want %v", run, sentinels, want)
		}
		var secrets []string
		if err := write.Model(&model.Secret{}).Order("id").Pluck("id", &secrets).Error; err != nil {
			t.Fatalf("run %d: list secrets: %v", run, err)
		}
		if want := []string{"sec-gh"}; !reflect.DeepEqual(secrets, want) {
			t.Fatalf("run %d: secrets = %v, want the gate gone", run, secrets)
		}

		var requests []model.SecretRequest
		if err := write.Order("purpose").Find(&requests).Error; err != nil {
			t.Fatalf("run %d: list requests: %v", run, err)
		}
		if len(requests) != 2 {
			t.Fatalf("run %d: requests = %+v, want the two sandbox grants asked again once", run, requests)
		}
		delegate, use := requests[0], requests[1]
		if use.SandboxID != "sbx-1" || use.Status != model.SecretRequestStatusPending || use.Purpose != model.SecretPurposeUse ||
			use.Name != "github" || use.EnvName != "GH_TOKEN" || !reflect.DeepEqual(use.Routes, []model.SecretRoute{upgraded("api.github.com", "", false)}) ||
			!strings.Contains(use.Justification, "open a PR") {
			t.Fatalf("run %d: use request = %+v", run, use)
		}
		if delegate.Purpose != model.SecretPurposeDelegate || delegate.Status != model.SecretRequestStatusPending ||
			!reflect.DeepEqual(delegate.Routes, []model.SecretRoute{upgraded("github.com", "", false)}) {
			t.Fatalf("run %d: delegate request = %+v", run, delegate)
		}

		var sandbox model.Sandbox
		if err := write.First(&sandbox, "id = ?", "sbx-1").Error; err != nil {
			t.Fatalf("run %d: read sandbox: %v", run, err)
		}
		if sandbox.DiscoboxAPI {
			t.Fatalf("run %d: a gate grant became the API flag; a person sets it", run)
		}

		migrator := db.Write.Migrator()
		for _, table := range []string{"credential_verdicts", "approval_rules"} {
			if migrator.HasTable(table) {
				t.Fatalf("run %d: table %s survived", run, table)
			}
		}
		for table, columns := range map[string][]string{
			"secret_requests":     {"uses", "policy", "well_known_id"},
			"secrets":             {"well_known_id"},
			"sandbox_secrets":     {"agent_requested"},
			"host_trusts":         {"uses"},
			"host_trust_requests": {"uses"},
			"projects":            {"judge_pool_id", "judge_sandbox_id"},
			"secret_rejections":   {"use_id"},
		} {
			for _, column := range columns {
				if migrator.HasColumn(table, column) {
					t.Fatalf("run %d: %s.%s survived", run, table, column)
				}
			}
		}
		// The judge reads as a run discobox being deleted, and is marked to be.
		var judge model.Sandbox
		if err := write.First(&judge, "id = ?", "sbx-judge").Error; err != nil {
			t.Fatalf("run %d: read judge: %v", run, err)
		}
		if judge.HarnessMode != "run" || judge.DesiredState != model.DesiredStateDeleted || judge.Generation == 0 {
			t.Fatalf("run %d: judge = mode %q, desired %q, generation %d; want a run discobox marked for deletion", run, judge.HarnessMode, judge.DesiredState, judge.Generation)
		}
		var trusts []string
		if err := write.Model(&model.HostTrust{}).Order("id").Pluck("id", &trusts).Error; err != nil {
			t.Fatal(err)
		}
		if want := []string{"ht-lapsed", "ht-pin"}; !reflect.DeepEqual(trusts, want) {
			t.Fatalf("run %d: trusts = %v, want the live judged one gone", run, trusts)
		}
		var marks []string
		if err := write.Table("reconcile_dirty").Order("resource_type, resource_id").
			Select("resource_type || ' ' || resource_id").Scan(&marks).Error; err != nil {
			t.Fatal(err)
		}
		// Spelled by the owners of the types and IDs, which this package sits
		// below and names as literals: a rename there fails here.
		mark := func(resourceType, sandboxID string) string {
			return resourceType + " " + sandboxes.SandboxDirtyID("project-1", sandboxID)
		}
		if want := []string{
			mark(sandboxes.SandboxResourceType, "sbx-judge"),
			mark(sandboxes.SentinelsResourceType, "sbx-1"),
			mark(sandboxes.SentinelsResourceType, "sbx-copilot"),
		}; !reflect.DeepEqual(marks, want) {
			t.Fatalf("run %d: dirty = %v, want the judge's purge and each holder's sentinels, and no retired type's work", run, marks)
		}

		// The variable is unique on the sandbox again, without the flag.
		if !migrator.HasIndex(&model.SandboxSecret{}, "idx_sandbox_secret_env") {
			t.Fatalf("run %d: the variable's uniqueness index is gone", run)
		}
	}
	if err := write.Create(&model.SandboxSecret{
		ID: "ss-dup", ProjectID: "project-1", SandboxID: "sbx-1", SecretID: "sec-gh", EnvName: "GH_TOKEN", Sentinel: "SENTINEL-DUP",
	}).Error; err == nil {
		t.Fatal("a second binding of the same variable was accepted; the index constrains nothing")
	}

	said := logged.String()
	for _, want := range []string{"host trust ht-judged", "judge discobox sbx-judge", "use grant g-agent", "delegate grant g-delegate", "use grant g-project", "gate secret sec-gate", "judged routes of secret sec-gh", "judged routes of harness_config hc-1"} {
		if !strings.Contains(said, want) {
			t.Errorf("log = %q, want it to say %q", said, want)
		}
	}
	if strings.Count(said, "took back") != 3 {
		t.Errorf("log = %q, want one line for each of the three grants taken back, once", said)
	}

	// The routes column holds the rewritten JSON, not a re-marshaled use.
	var raw string
	if err := write.Table("secrets").Select("routes").Where("id = ?", "sec-gh").Scan(&raw).Error; err != nil {
		t.Fatal(err)
	}
	var routes []map[string]any
	if err := json.Unmarshal([]byte(raw), &routes); err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		if _, ok := route["use"]; ok {
			t.Fatalf("routes = %s, want no use left", raw)
		}
	}
}
