package database_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/x/gormdb"
)

// A server from before archive closed a sandbox's open asks left them pending
// for agents that were gone. The upgrade denies, by system, every open secret,
// host trust and API access request of a sandbox archived, being deleted or
// purged, and every open adoption request with such a sandbox on either side.
// It leaves a live sandbox's open asks, and every answered one, alone.
func TestMigrateDeniesTheOpenRequestsOfArchivedAndGoneSandboxes(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{Driver: gormdb.DriverSQLite, DSN: "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}
	rows := []any{
		&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"},
		&model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "Docker"},
		&model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: "provider-1"}},
	}
	for id, desired := range map[string]string{"live": model.DesiredStatePresent, "archived": model.DesiredStateArchived, "deleting": model.DesiredStateDeleted} {
		sb := &model.Sandbox{ID: id, ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: id}
		sb.DesiredState = desired
		rows = append(rows, sb)
	}
	for _, row := range rows {
		if err := db.Write.Create(row).Error; err != nil {
			t.Fatalf("create %T: %v", row, err)
		}
	}
	type ask struct{ sandbox, status, want string }
	asks := map[string]ask{
		"live-open":         {"live", model.SecretRequestStatusPending, model.SecretRequestStatusPending},
		"archived-open":     {"archived", model.SecretRequestStatusPending, model.SecretRequestStatusDenied},
		"archived-approved": {"archived", model.SecretRequestStatusApproved, model.SecretRequestStatusApproved},
		"deleting-open":     {"deleting", model.SecretRequestStatusPending, model.SecretRequestStatusDenied},
		"purged-open":       {"purged", model.SecretRequestStatusPending, model.SecretRequestStatusDenied},
	}
	for id, a := range asks {
		if err := db.Write.Create(&model.APIAccessRequest{ID: "api-" + id, ProjectID: "project-1", RequesterSandboxID: a.sandbox, Justification: "x", Status: a.status}).Error; err != nil {
			t.Fatal(err)
		}
		req := &model.SecretRequest{
			ID: "secret-" + id, ProjectID: "project-1", RequestedBy: "agent:" + a.sandbox, SandboxID: a.sandbox,
			Use:    model.SecretRequestPart{Asked: true, Routes: []model.SecretRoute{{Hosts: []string{"api.example.com"}}}},
			Status: a.status,
		}
		if err := db.Write.Create(req).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Write.Create(&model.HostTrustRequest{ID: "trust-" + id, ProjectID: "project-1", SandboxID: a.sandbox, RequestedBy: "agent:" + a.sandbox, Host: "h:443", Status: a.status}).Error; err != nil {
			t.Fatal(err)
		}
		// Adoptions with the sandbox on each side, the other side live.
		for side, ids := range map[string][2]string{"requester": {a.sandbox, "live"}, "child": {"live", a.sandbox}} {
			if err := db.Write.Create(&model.AdoptionRequest{ID: "adopt-" + side + "-" + id, ProjectID: "project-1", RequesterSandboxID: ids[0], ChildSandboxID: ids[1], Justification: "x", Status: a.status}).Error; err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for id, a := range asks {
		var api model.APIAccessRequest
		if err := db.Write.First(&api, "id = ?", "api-"+id).Error; err != nil {
			t.Fatal(err)
		}
		var secret model.SecretRequest
		if err := db.Write.First(&secret, "id = ?", "secret-"+id).Error; err != nil {
			t.Fatal(err)
		}
		var trust model.HostTrustRequest
		if err := db.Write.First(&trust, "id = ?", "trust-"+id).Error; err != nil {
			t.Fatal(err)
		}
		statuses := map[string]string{"API access": api.Status, "secret": secret.Status, "host trust": trust.Status}
		for _, side := range []string{"requester", "child"} {
			var adoption model.AdoptionRequest
			if err := db.Write.First(&adoption, "id = ?", "adopt-"+side+"-"+id).Error; err != nil {
				t.Fatal(err)
			}
			statuses["adoption as "+side] = adoption.Status
		}
		for kind, got := range statuses {
			if got != a.want {
				t.Errorf("%s request %s = %s, want %s", kind, id, got, a.want)
			}
		}
		if a.status == model.SecretRequestStatusPending && a.want == model.SecretRequestStatusDenied {
			if api.DecidedBy != model.SecretBindingActorSystem || api.DecidedAt == nil {
				t.Errorf("API access request %s decided by %q at %v, want by system", id, api.DecidedBy, api.DecidedAt)
			}
			if secret.DecidedBy != model.SecretBindingActorSystem || secret.Use.Outcome != model.SecretRequestPartDenied {
				t.Errorf("secret request %s decided by %q, use part %q, want denied by system", id, secret.DecidedBy, secret.Use.Outcome)
			}
		}
	}
}
