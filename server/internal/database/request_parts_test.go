package database_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/x/gormdb"
)

// A request from before parts asked for one purpose with one route list. The
// upgrade gives it the one part it asked for, carrying its routes, the binding
// approving it made and its answer, and drops the old columns; it runs on
// every start and finds nothing the second time (ADR 26-11-02-022 §6).
func TestMigrateSplitsSecretRequestsIntoParts(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{Driver: gormdb.DriverSQLite, DSN: "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}
	// The sandbox that asked is live, so its pending ask stays pending.
	for _, row := range []any{
		&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"},
		&model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "Docker"},
		&model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: "provider-1"}},
		&model.Sandbox{ID: "sb-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "sb-1"},
	} {
		if err := db.Write.Create(row).Error; err != nil {
			t.Fatalf("create %T: %v", row, err)
		}
	}
	// The table as it was: one purpose, one route list, one binding.
	for _, sql := range []string{
		"ALTER TABLE secret_requests ADD COLUMN purpose text NOT NULL DEFAULT 'use'",
		"ALTER TABLE secret_requests ADD COLUMN routes text",
		"ALTER TABLE secret_requests ADD COLUMN binding_id text NOT NULL DEFAULT ''",
	} {
		if err := db.Write.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	const routes = `[{"hosts":["api.github.com"]}]`
	for _, row := range []struct{ id, purpose, status, binding string }{
		{"r-use-pending", "use", model.SecretRequestStatusPending, ""},
		{"r-use-approved", "use", model.SecretRequestStatusApproved, "bind-use"},
		{"r-delegate-denied", "delegate", model.SecretRequestStatusDenied, ""},
		{"r-delegate-approved", "delegate", model.SecretRequestStatusApproved, "bind-delegate"},
	} {
		if err := db.Write.Exec(`INSERT INTO secret_requests (id, project_id, requested_by, sandbox_id, name, env_name, purpose, routes, binding_id, status, created_at, updated_at)
			VALUES (?, 'project-1', 'agent:sb-1', 'sb-1', 'github', ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
			row.id, map[bool]string{true: "GH_TOKEN"}[row.purpose == "use"], row.purpose, routes, row.binding, row.status).Error; err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}

	for range 2 {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	for _, column := range []string{"purpose", "routes", "binding_id"} {
		if db.Write.Migrator().HasColumn("secret_requests", column) {
			t.Errorf("secret_requests.%s survived the upgrade", column)
		}
	}
	want := []model.SecretRoute{{Hosts: []string{"api.github.com"}}}
	part := func(outcome, binding string) model.SecretRequestPart {
		return model.SecretRequestPart{Asked: true, Routes: want, Outcome: outcome, BindingID: binding}
	}
	replaced := func(p model.SecretRequestPart) model.SecretRequestPart {
		p.ApprovedAs = model.SecretRequestApprovedAsReplace
		return p
	}
	for id, expect := range map[string]struct{ use, delegate model.SecretRequestPart }{
		"r-use-pending": {use: part("", "")},
		// Every approval before additions replaced (ADR 26-11-03-329 §4).
		"r-use-approved":      {use: replaced(part(model.SecretRequestPartApproved, "bind-use"))},
		"r-delegate-denied":   {delegate: part(model.SecretRequestPartDenied, "")},
		"r-delegate-approved": {delegate: part(model.SecretRequestPartApproved, "bind-delegate")},
	} {
		var got model.SecretRequest
		if err := db.Write.First(&got, "id = ?", id).Error; err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if !reflect.DeepEqual(got.Use, expect.use) || !reflect.DeepEqual(got.Delegate, expect.delegate) {
			t.Errorf("%s: use %+v, delegate %+v; want use %+v, delegate %+v", id, got.Use, got.Delegate, expect.use, expect.delegate)
		}
	}
}

// An upgrade stopped after the moves and the first drops is finished by the
// next start: purpose, dropped last, still marks the step as not done, and
// the moves already made are not read again from columns that are gone.
func TestMigrateFinishesAnInterruptedSplit(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{Driver: gormdb.DriverSQLite, DSN: "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}
	// Moved, routes and binding_id dropped, purpose left behind.
	if err := db.Write.Exec("ALTER TABLE secret_requests ADD COLUMN purpose text NOT NULL DEFAULT 'use'").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if db.Write.Migrator().HasColumn("secret_requests", "purpose") {
		t.Error("secret_requests.purpose survived the finishing start")
	}
}
