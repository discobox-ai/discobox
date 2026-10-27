package database_test

import (
	"bytes"
	"context"
	"fmt"
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
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/x/gormdb"
)

// grantEraSchema puts back what a database written while grants authorized
// credentials carried (ADR 26-10-27-880 §8): the grant and harness binding
// tables, a secret's host and the key in its old uniqueness index, the
// request's old shape, the sentinel's harness mark, and the limit under its
// grant name.
func grantEraSchema(t *testing.T, db *database.DB) {
	t.Helper()
	for _, stmt := range []string{
		`DROP INDEX idx_secret_project_name`,
		`ALTER TABLE secrets RENAME COLUMN max_binding_ttl_seconds TO max_grant_ttl_seconds`,
		`ALTER TABLE secrets ADD COLUMN host text NOT NULL DEFAULT ''`,
		`ALTER TABLE secrets ADD COLUMN unique_key text NOT NULL DEFAULT ''`,
		`CREATE UNIQUE INDEX idx_secret_project_type_host ON secrets (project_id, name, type, host, unique_key)`,
		`ALTER TABLE secret_requests ADD COLUMN type text NOT NULL DEFAULT 'token'`,
		`ALTER TABLE secret_requests ADD COLUMN hosts text`,
		`ALTER TABLE secret_requests ADD COLUMN grant_ttl_seconds integer NOT NULL DEFAULT 0`,
		`ALTER TABLE secret_requests ADD COLUMN grant_id text NOT NULL DEFAULT ''`,
		`ALTER TABLE secret_requests ADD COLUMN reason text NOT NULL DEFAULT ''`,
		`ALTER TABLE secret_requests ADD COLUMN refresh_cause text NOT NULL DEFAULT ''`,
		`ALTER TABLE secret_requests ADD COLUMN refresh_answer text`,
		`ALTER TABLE secret_requests ADD COLUMN closed_at datetime`,
		`CREATE UNIQUE INDEX idx_secret_request_open_refresh ON secret_requests (project_id, secret_id) WHERE reason = 'refresh' AND status = 'pending'`,
		`ALTER TABLE sandbox_secrets ADD COLUMN source_harness_config_id text NOT NULL DEFAULT ''`,
		`CREATE TABLE secret_grants (id text PRIMARY KEY, project_id text NOT NULL, secret_id text NOT NULL, scope text NOT NULL,
			scope_key text NOT NULL, hosts text, granted_by text NOT NULL DEFAULT '', granted_at datetime, expires_at datetime,
			purpose text NOT NULL DEFAULT 'use', delegation_id text NOT NULL DEFAULT '', created_at datetime, updated_at datetime)`,
		`CREATE TABLE harness_config_secret_bindings (id text PRIMARY KEY, project_id text NOT NULL, harness_config_id text NOT NULL,
			env_name text NOT NULL, secret_id text NOT NULL, created_at datetime, updated_at datetime)`,
	} {
		if err := db.Write.Exec(stmt).Error; err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// openGrantEra opens a database at the grant era's schema, with a project, a
// pool and the engine's table, as a server that ran one has them.
func openGrantEra(t *testing.T) *database.DB {
	t.Helper()
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
	if _, err := reconcile.New(db.Write, reconcile.Options{}); err != nil {
		t.Fatalf("reconcile table: %v", err)
	}
	grantEraSchema(t, db)
	write := db.Write.WithContext(ctx)
	for _, row := range []any{
		&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"},
		&model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "Docker"},
		&model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: "provider-1"}},
	} {
		if err := write.Create(row).Error; err != nil {
			t.Fatalf("create %T: %v", row, err)
		}
	}
	return db
}

// Upgrading from grants makes a binding of every grant that authorized
// something and widens nothing (ADR 26-10-27-880 §8, ADR 26-10-27-939 §3): a
// secret's host becomes its routes, or `*` when it had none; a harness
// binding a grant covered is bound on its harness config with the grant's
// hosts and expiry, and one none covered goes; a sandbox's own sentinel a
// grant covered is bound on it, one none covered is asked for again; a
// delegation is a delegate binding and what was handed on under it its child;
// refresh requests move to their own table and the proxy's own requests go.
// Every binding made is recorded, every sandbox holding a sentinel is marked,
// the retired tables and columns are gone, and a second run changes nothing.
func TestMigrateRetiresSecretGrants(t *testing.T) {
	ctx := context.Background()
	db := openGrantEra(t)
	write := db.Write.WithContext(ctx)
	live := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	lapsed := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	hc := "hc-1"
	for _, row := range []any{
		&model.HarnessConfig{ID: hc, ProjectID: "project-1", Slug: "claude", Name: "Claude"},
		&model.Sandbox{ID: "sbx-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "one", SandboxManifest: model.SandboxManifest{HarnessConfigID: &hc}},
		&model.Sandbox{ID: "sbx-2", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "two"},
	} {
		if err := write.Create(row).Error; err != nil {
			t.Fatalf("create %T: %v", row, err)
		}
	}
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO secrets (id, project_id, name, type, host, unique_key, max_grant_ttl_seconds, routes) VALUES
			('sec_host', 'project-1', 'github', 'token', 'api.github.com', '', 900, NULL),
			('sec_none', 'project-1', 'openai', 'token', '', '', 3600, NULL),
			('sec_both', 'project-1', 'gitlab', 'token', 'api.gitlab.com', '', 0, '[{"host":"gitlab.com","path":"/api"},{"host":"example.com"},{"host":"api.gitlab.com","path":"/v4","blocked":true}]'),
			('sec_dup1', 'project-1', 'dup', 'token', 'a.example', '', 0, '[{"host":"a.example"}]'),
			('sec_dup2', 'project-1', 'dup', 'token', 'b.example', '', 0, '[{"host":"b.example"}]')`, nil},
		{`UPDATE secrets SET created_at = ? WHERE id = 'sec_dup1'`, []any{lapsed}},
		{`UPDATE secrets SET created_at = ? WHERE id = 'sec_dup2'`, []any{live}},
		{`INSERT INTO harness_config_secret_bindings (id, project_id, harness_config_id, env_name, secret_id) VALUES
			('hcsb_covered', 'project-1', 'hc-1', 'GH_TOKEN', 'sec_host'),
			('hcsb_uncovered', 'project-1', 'hc-1', 'GITLAB_HARNESS', 'sec_both')`, nil},
		{`INSERT INTO secret_grants (id, project_id, secret_id, scope, scope_key, hosts, purpose, delegation_id, expires_at) VALUES
			('grant_project', 'project-1', 'sec_host', 'project', 'project-1', '["api.github.com"]', 'use', '', ?),
			('grant_wild', 'project-1', 'sec_none', 'sandbox', 'sbx-1', NULL, 'use', '', NULL),
			('grant_delegate', 'project-1', 'sec_both', 'sandbox', 'sbx-1', '["gitlab.com"]', 'delegate', '', ?),
			('grant_child', 'project-1', 'sec_both', 'sandbox', 'sbx-2', '["gitlab.com"]', 'use', 'grant_delegate', ?),
			('grant_lapsed', 'project-1', 'sec_dup1', 'sandbox', 'sbx-2', '["a.example"]', 'use', '', ?)`, []any{live, live, live, lapsed}},
		{`INSERT INTO sandbox_secrets (id, project_id, sandbox_id, secret_id, env_name, sentinel, source_harness_config_id) VALUES
			('ss_harness', 'project-1', 'sbx-1', 'sec_host', 'GH_TOKEN', 'SENTINEL-H', 'hc-1'),
			('ss_unsent', 'project-1', 'sbx-1', 'sec_both', 'GITLAB_HARNESS', 'SENTINEL-U', 'hc-1'),
			('ss_extra', 'project-1', 'sbx-1', 'sec_none', 'EXTRA_KEY', 'SENTINEL-E', ''),
			('ss_ungranted', 'project-1', 'sbx-1', 'sec_both', 'GITLAB_TOKEN', 'SENTINEL-G', ''),
			('ss_child', 'project-1', 'sbx-2', 'sec_both', 'GITLAB_TOKEN', 'SENTINEL-C', ''),
			('ss_lapsed', 'project-1', 'sbx-2', 'sec_dup1', 'DUP_TOKEN', 'SENTINEL-L', '')`, nil},
		{`INSERT INTO secret_requests (id, project_id, requested_by, sandbox_id, secret_id, name, env_name, hosts, grant_ttl_seconds, grant_id, purpose, status, reason, refresh_cause, closed_at, created_at, updated_at) VALUES
			('req_refresh', 'project-1', 'sandbox:sbx-1', 'sbx-1', 'sec_host', 'github', '', '["api.github.com"]', 0, '', 'use', 'pending', 'refresh', 'rejected', NULL, ?, ?),
			('req_refreshed', 'project-1', 'sandbox:sbx-1', 'sbx-1', 'sec_none', 'openai', '', NULL, 0, '', 'use', 'approved', 'refresh', 'stale', ?, ?, ?),
			('req_reactive', 'project-1', 'sandbox:sbx-1', 'sbx-1', 'sec_host', '', '', '["api.github.com"]', 0, '', 'use', 'pending', '', '', NULL, ?, ?),
			('req_ask', 'project-1', 'agent:sbx-2', 'sbx-2', '', 'github', 'GH_TOKEN', '["api.github.com"]', 600, '', 'use', 'pending', '', '', NULL, ?, ?),
			('req_approved', 'project-1', 'agent:sbx-1', 'sbx-1', 'sec_none', 'openai', 'EXTRA_KEY', '["api.openai.com"]', 0, 'grant_wild', 'use', 'approved', '', '', ?, ?, ?)`,
			[]any{lapsed, lapsed, live, lapsed, lapsed, lapsed, lapsed, lapsed, lapsed, live, lapsed, lapsed}},
	} {
		if err := write.Exec(stmt.sql, stmt.args...).Error; err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt.sql)
		}
	}

	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })

	for run := 1; run <= 2; run++ {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("upgrade migrate %d: %v", run, err)
		}

		secrets := map[string]model.Secret{}
		var rows []model.Secret
		if err := write.Find(&rows).Error; err != nil {
			t.Fatalf("run %d: read secrets: %v", run, err)
		}
		for _, row := range rows {
			secrets[row.ID] = row
		}
		for id, want := range map[string][]model.SecretRoute{
			"sec_host": {{Host: "api.github.com"}},
			"sec_none": {{Host: "*"}},
			// Narrowed to the host, never widened: the route above it is
			// brought down to it, the one beside it goes.
			"sec_both": {{Host: "api.gitlab.com", Path: "/api"}, {Host: "api.gitlab.com", Path: "/v4", Blocked: true}},
		} {
			if got := secrets[id].Routes; !reflect.DeepEqual(got, want) {
				t.Fatalf("run %d: %s routes = %+v, want %+v", run, id, got, want)
			}
		}
		if secrets["sec_host"].MaxBindingTTL != 900 {
			t.Fatalf("run %d: limit = %d, want the 900 the grant limit carried", run, secrets["sec_host"].MaxBindingTTL)
		}
		if secrets["sec_dup1"].Name != "dup" || secrets["sec_dup2"].Name != "dup-2" {
			t.Fatalf("run %d: duplicate names = %q, %q; want the older kept and the newer suffixed", run, secrets["sec_dup1"].Name, secrets["sec_dup2"].Name)
		}

		var bindings []model.SecretBinding
		if err := write.Order("id").Find(&bindings).Error; err != nil {
			t.Fatalf("run %d: list bindings: %v", run, err)
		}
		byID := map[string]model.SecretBinding{}
		for _, binding := range bindings {
			byID[binding.ID] = binding
		}
		if len(bindings) != 4 {
			t.Fatalf("run %d: bindings = %+v, want four", run, bindings)
		}
		harness := byID["bind_covered"]
		if harness.TargetKind != model.SecretBindingTargetHarnessConfig || harness.TargetID != hc || !harness.Use || harness.EnvName != "GH_TOKEN" ||
			!reflect.DeepEqual(harness.Routes, []model.SecretRoute{{Host: "api.github.com"}}) || harness.ExpiresAt == nil || !harness.ExpiresAt.Equal(live) {
			t.Fatalf("run %d: harness binding = %+v", run, harness)
		}
		extra := byID["bind_extra"]
		if extra.TargetKind != model.SecretBindingTargetSandbox || extra.TargetID != "sbx-1" || extra.EnvName != "EXTRA_KEY" ||
			!reflect.DeepEqual(extra.Routes, []model.SecretRoute{{Host: "*"}}) || extra.ExpiresAt != nil {
			t.Fatalf("run %d: wildcard grant's binding = %+v, want `*` and no expiry", run, extra)
		}
		delegate := byID["bind_delegate"]
		if delegate.TargetID != "sbx-1" || !delegate.Delegate || delegate.Use || delegate.EnvName != "" {
			t.Fatalf("run %d: delegate binding = %+v", run, delegate)
		}
		child := byID["bind_child"]
		if child.TargetID != "sbx-2" || !child.Use || child.ParentID != "bind_delegate" || child.EnvName != "GITLAB_TOKEN" {
			t.Fatalf("run %d: handed-on binding = %+v, want a child of the delegate binding", run, child)
		}

		var events []model.SecretBindingEvent
		if err := write.Find(&events).Error; err != nil {
			t.Fatal(err)
		}
		if len(events) != len(bindings) {
			t.Fatalf("run %d: events = %d, want one per binding", run, len(events))
		}
		for _, event := range events {
			if event.Event != model.SecretBindingEventCreated || event.Actor != model.SecretBindingActorSystem || event.Cause != "upgrade" {
				t.Fatalf("run %d: event = %+v, want created by the system for the upgrade", run, event)
			}
		}

		var sentinels []model.SandboxSecret
		if err := write.Order("id").Find(&sentinels).Error; err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, sentinel := range sentinels {
			got[sentinel.ID] = sentinel.BindingID
		}
		if want := map[string]string{"ss_harness": "bind_covered", "ss_extra": "bind_extra", "ss_child": "bind_child"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: sentinels = %v, want %v", run, got, want)
		}

		var requests []model.SecretRequest
		if err := write.Order("id").Find(&requests).Error; err != nil {
			t.Fatal(err)
		}
		requestByID := map[string]model.SecretRequest{}
		for _, request := range requests {
			requestByID[request.ID] = request
		}
		if _, ok := requestByID["req_reactive"]; ok {
			t.Fatalf("run %d: the proxy's own request survived", run)
		}
		ask := requestByID["req_ask"]
		if ask.Status != model.SecretRequestStatusPending || ask.TTL != 600 || !reflect.DeepEqual(ask.Routes, []model.SecretRoute{{Host: "api.github.com"}}) {
			t.Fatalf("run %d: agent request = %+v", run, ask)
		}
		approved := requestByID["req_approved"]
		if approved.BindingID != "bind_extra" || approved.DecidedAt == nil {
			t.Fatalf("run %d: approved request = %+v, want the binding its grant became", run, approved)
		}
		var reasked []model.SecretRequest
		for _, request := range requests {
			if strings.HasPrefix(request.ID, "req_") {
				continue
			}
			reasked = append(reasked, request)
		}
		if len(reasked) != 2 {
			t.Fatalf("run %d: requests asked again = %+v, want the ungranted and the lapsed", run, reasked)
		}
		for _, request := range reasked {
			if request.Status != model.SecretRequestStatusPending || request.RequestedBy != "agent:"+request.SandboxID {
				t.Fatalf("run %d: request asked again = %+v", run, request)
			}
		}

		var refreshes []model.SecretRefreshRequest
		if err := write.Order("id").Find(&refreshes).Error; err != nil {
			t.Fatal(err)
		}
		if len(refreshes) != 2 || refreshes[0].ID != "req_refresh" || refreshes[0].Status != model.SecretRefreshStatusPending ||
			refreshes[0].Cause != model.SecretRefreshCauseRejected || refreshes[1].Status != model.SecretRefreshStatusAnswered || refreshes[1].ClosedAt == nil {
			t.Fatalf("run %d: refresh requests = %+v", run, refreshes)
		}

		var marks []string
		if err := write.Table("reconcile_dirty").Where("resource_type = ?", sandboxes.SentinelsResourceType).
			Order("resource_id").Pluck("resource_id", &marks).Error; err != nil {
			t.Fatal(err)
		}
		if want := []string{sandboxes.SandboxDirtyID("project-1", "sbx-1"), sandboxes.SandboxDirtyID("project-1", "sbx-2")}; !reflect.DeepEqual(marks, want) {
			t.Fatalf("run %d: sentinel marks = %v, want every sandbox holding a sentinel", run, marks)
		}

		migrator := db.Write.Migrator()
		for _, table := range []string{"secret_grants", "harness_config_secret_bindings"} {
			if migrator.HasTable(table) {
				t.Fatalf("run %d: table %s survived", run, table)
			}
		}
		for table, columns := range map[string][]string{
			"secrets":         {"host", "unique_key", "max_grant_ttl_seconds"},
			"secret_requests": {"type", "hosts", "grant_ttl_seconds", "grant_id", "reason", "refresh_cause", "refresh_answer", "closed_at"},
			"sandbox_secrets": {"source_harness_config_id"},
		} {
			for _, column := range columns {
				if migrator.HasColumn(table, column) {
					t.Fatalf("run %d: %s.%s survived", run, table, column)
				}
			}
		}
	}

	// A secret's name is now the whole of its uniqueness.
	if err := write.Create(&model.Secret{ID: "sec_dup3", ProjectID: "project-1", Name: "dup", Type: model.SecretTypeToken}).Error; err == nil {
		t.Fatal("a second secret named dup was accepted; the name is not unique")
	}
	said := logged.String()
	for _, want := range []string{`"dup" to "dup-2"`, "sec_none of project project-1 had no host and no routes", "dropped harness config hc-1's binding of GITLAB_HARNESS", "deleted request req_reactive"} {
		if !strings.Contains(said, want) {
			t.Errorf("log = %q, want it to say %q", said, want)
		}
	}
}

// Several live grants on one variable become one binding taking all their
// hosts — `*` when one had none — and the earliest expiry, logged: old
// resolves accepted any of them, so no host one of them allowed is lost.
func TestMigrateMergesTheGrantsOfOneVariable(t *testing.T) {
	ctx := context.Background()
	db := openGrantEra(t)
	write := db.Write.WithContext(ctx)
	sooner := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	later := sooner.Add(time.Hour)
	if err := write.Create(&model.Sandbox{ID: "sbx-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "one"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO secrets (id, project_id, name, type, max_grant_ttl_seconds) VALUES ('sec_gh', 'project-1', 'github', 'token', 0), ('sec_any', 'project-1', 'any', 'token', 0)`, nil},
		{`INSERT INTO secret_grants (id, project_id, secret_id, scope, scope_key, hosts, purpose, expires_at) VALUES
			('grant_api', 'project-1', 'sec_gh', 'sandbox', 'sbx-1', '["api.github.com"]', 'use', ?),
			('grant_uploads', 'project-1', 'sec_gh', 'sandbox', 'sbx-1', '["uploads.github.com"]', 'use', ?),
			('grant_project', 'project-1', 'sec_any', 'project', 'project-1', '["a.example"]', 'use', NULL),
			('grant_wild', 'project-1', 'sec_any', 'sandbox', 'sbx-1', NULL, 'use', NULL)`, []any{later, sooner}},
		{`INSERT INTO sandbox_secrets (id, project_id, sandbox_id, secret_id, env_name, sentinel) VALUES
			('ss_gh', 'project-1', 'sbx-1', 'sec_gh', 'GH_TOKEN', 'S-GH'), ('ss_any', 'project-1', 'sbx-1', 'sec_any', 'ANY', 'S-ANY')`, nil},
	} {
		if err := write.Exec(stmt.sql, stmt.args...).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var gh, anyBinding model.SecretBinding
	if err := write.First(&gh, "id = ?", "bind_gh").Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gh.Routes, []model.SecretRoute{{Host: "api.github.com"}, {Host: "uploads.github.com"}}) ||
		gh.ExpiresAt == nil || !gh.ExpiresAt.Equal(sooner) {
		t.Fatalf("binding = %+v, want both grants' hosts and the earliest expiry", gh)
	}
	if err := write.First(&anyBinding, "id = ?", "bind_any").Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(anyBinding.Routes, []model.SecretRoute{{Host: "*"}}) || anyBinding.ExpiresAt != nil {
		t.Fatalf("binding = %+v, want `*` since one grant named no host", anyBinding)
	}
	if !strings.Contains(logged.String(), "covered by 2 grants (grant_api, grant_uploads)") {
		t.Fatalf("log = %q, want the merge said", logged.String())
	}
}

// Narrowing a route above a secret's host down to it never unblocks: an open
// route that would then outrank a blocked carve-out it used to lose to is
// dropped rather than narrowed; one the carve-out still answers within is
// narrowed and kept.
func TestMigrateNarrowingNeverUnblocks(t *testing.T) {
	ctx := context.Background()
	db := openGrantEra(t)
	write := db.Write.WithContext(ctx)
	if err := write.Exec(`INSERT INTO secrets (id, project_id, name, type, host, max_grant_ttl_seconds, routes) VALUES
		('sec_x', 'project-1', 'x', 'token', 'a.b.com', 0, '[{"host":"b.com","path":"/x/y"},{"host":"a.b.com","path":"/x","blocked":true},{"host":"b.com","path":"/z"}]'),
		('sec_kept', 'project-1', 'kept', 'token', 'api.x.example', 0, '[{"host":"x.example"},{"host":"api.x.example","path":"/admin","blocked":true}]')`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var secret model.Secret
	if err := write.First(&secret, "id = ?", "sec_x").Error; err != nil {
		t.Fatal(err)
	}
	want := []model.SecretRoute{{Host: "a.b.com", Path: "/x", Blocked: true}, {Host: "a.b.com", Path: "/z"}}
	if !reflect.DeepEqual(secret.Routes, want) {
		t.Fatalf("routes = %+v, want %+v: /x/y would have outranked the blocked /x", secret.Routes, want)
	}
	var kept model.Secret
	if err := write.First(&kept, "id = ?", "sec_kept").Error; err != nil {
		t.Fatal(err)
	}
	if want := []model.SecretRoute{{Host: "api.x.example"}, {Host: "api.x.example", Path: "/admin", Blocked: true}}; !reflect.DeepEqual(kept.Routes, want) {
		t.Fatalf("routes = %+v, want %+v: the block still answers for /admin, and /foo stays allowed", kept.Routes, want)
	}
}

// The upgrade arms the expiry sweep under the store's own spelling, which
// this package names as literals because it sits below the store.
func TestMigrateArmsTheExpirySweepAsTheStoreDoes(t *testing.T) {
	ctx := context.Background()
	db := openGrantEra(t)
	write := db.Write.WithContext(ctx)
	expires := time.Now().UTC().Add(time.Hour)
	if err := write.Create(&model.Sandbox{ID: "sbx-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "one"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO secrets (id, project_id, name, type, max_grant_ttl_seconds) VALUES ('sec_gh', 'project-1', 'github', 'token', 0)`, nil},
		{`INSERT INTO secret_grants (id, project_id, secret_id, scope, scope_key, hosts, purpose, expires_at) VALUES ('grant_1', 'project-1', 'sec_gh', 'sandbox', 'sbx-1', '["github.com"]', 'use', ?)`, []any{expires}},
		{`INSERT INTO sandbox_secrets (id, project_id, sandbox_id, secret_id, env_name, sentinel) VALUES ('ss_gh', 'project-1', 'sbx-1', 'sec_gh', 'GH_TOKEN', 'S-GH')`, nil},
	} {
		if err := write.Exec(stmt.sql, stmt.args...).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var marks int64
	if err := write.Table("reconcile_dirty").Where("resource_type = ? AND resource_id = ?", store.BindingExpiryResourceType, store.BindingExpiryID).Count(&marks).Error; err != nil {
		t.Fatal(err)
	}
	if marks != 1 {
		t.Fatalf("expiry marks = %d, want the sweep armed for the binding's expiry", marks)
	}
}

func TestMigrateMergesDuplicateDelegations(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint("partial=", partial), func(t *testing.T) {
			db := openGrantEra(t)
			early := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
			late := early.Add(time.Hour)
			for _, stmt := range []string{
				`INSERT INTO secrets (id, project_id, name, type, max_grant_ttl_seconds) VALUES ('sec_dup', 'project-1', 'dup', 'token', 0)`,
				`INSERT INTO secret_grants (id, project_id, secret_id, scope, scope_key, hosts, purpose, expires_at) VALUES ('grant_a', 'project-1', 'sec_dup', 'sandbox', 'sbx-parent', '["github.com"]', 'delegate', ?)`,
				`INSERT INTO secret_grants (id, project_id, secret_id, scope, scope_key, hosts, purpose, expires_at) VALUES ('grant_b', 'project-1', 'sec_dup', 'sandbox', 'sbx-parent', '["gitlab.com"]', 'delegate', ?)`,
			} {
				args := []any{}
				if strings.Contains(stmt, "grant_a") {
					args = append(args, late)
				}
				if strings.Contains(stmt, "grant_b") {
					args = append(args, early)
				}
				if err := db.Write.Exec(stmt, args...).Error; err != nil {
					t.Fatal(err)
				}
			}
			for _, grantID := range []string{"grant_a", "grant_b"} {
				if err := db.Write.Exec(`INSERT INTO secret_requests
                    (id, project_id, requested_by, sandbox_id, secret_id, name, env_name, grant_id, purpose, status)
                    VALUES (?, 'project-1', 'agent:sbx-parent', 'sbx-parent', 'sec_dup', 'dup', '', ?, 'delegate', 'approved')`, "req_"+grantID, grantID).Error; err != nil {
					t.Fatal(err)
				}
			}
			if partial {
				if err := db.Write.Create(&model.SecretBinding{ID: "bind_a", ProjectID: "project-1", SecretID: "sec_dup", TargetKind: model.SecretBindingTargetSandbox, TargetID: "sbx-parent", Delegate: true, Routes: []model.SecretRoute{{Host: "github.com"}}, ExpiresAt: &late, CreatedBy: model.SecretBindingActorSystem}).Error; err != nil {
					t.Fatal(err)
				}
			}
			for run := 0; run < 2; run++ {
				if err := db.Migrate(context.Background()); err != nil {
					t.Fatal(err)
				}
				var bindings []model.SecretBinding
				if err := db.Write.Find(&bindings).Error; err != nil {
					t.Fatal(err)
				}
				if len(bindings) != 1 {
					t.Fatalf("bindings: %+v", bindings)
				}
				got := bindings[0]
				if got.ID != "bind_a" || !got.Delegate || got.Use || got.ExpiresAt == nil || !got.ExpiresAt.Equal(early) || !reflect.DeepEqual(got.Routes, []model.SecretRoute{{Host: "github.com"}, {Host: "gitlab.com"}}) {
					t.Fatalf("binding: %+v", got)
				}
				var requests []model.SecretRequest
				if err := db.Write.Find(&requests).Error; err != nil {
					t.Fatal(err)
				}
				if len(requests) != 2 {
					t.Fatalf("requests: %+v", requests)
				}
				for _, request := range requests {
					if request.BindingID != got.ID {
						t.Fatalf("request lost its binding: %+v", request)
					}
				}
				var events []model.SecretBindingEvent
				if err := db.Write.Find(&events).Error; err != nil {
					t.Fatal(err)
				}
				if len(events) != 1 {
					t.Fatalf("events: %+v", events)
				}
			}
		})
	}
}
