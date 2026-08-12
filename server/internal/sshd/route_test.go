package sshd

import (
	"context"
	"testing"

	idpkg "github.com/obot-platform/discobox/id"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/store"
)

func TestResolveUsername(t *testing.T) {
	ctx := context.Background()
	db := newRouteTestStore(t)

	acme := createRouteFixtureProject(t, db, "proj_acme00000000", "Acme")
	other := createRouteFixtureProject(t, db, "proj_other0000000", "Other Co")
	sandbox := createRouteFixtureSandbox(t, db, acme, "devbox")
	otherSandbox := createRouteFixtureSandbox(t, db, other, "devbox")

	// A short, distinctive prefix of the generated ID — what a user would
	// actually type, per id.ResolveShort's "prefix of the random part" rule.
	sandboxShort := idpkg.RandomPart(sandbox.ID)[:6]

	tests := []struct {
		name          string
		username      string
		wantProjectID string
		wantSandboxID string
		wantErr       bool
	}{
		{name: "exact sbx_ id", username: sandbox.ID, wantProjectID: acme, wantSandboxID: sandbox.ID},
		{name: "sbx_ id prefix", username: sandbox.ID[:len(sandbox.ID)-4], wantProjectID: acme, wantSandboxID: sandbox.ID},
		// Slugs are gone: a project is addressed by ID, and by name as the
		// convenience the name's per-owner uniqueness makes safe.
		{name: "sandbox short id . project name", username: sandboxShort + "." + "Acme", wantProjectID: acme, wantSandboxID: sandbox.ID},
		{name: "sandbox scoped to the wrong project does not resolve", username: sandboxShort + "." + "Other Co", wantErr: true},
		{name: "project name containing the split character", username: idpkg.RandomPart(otherSandbox.ID)[:6] + "." + "Other Co", wantProjectID: other, wantSandboxID: otherSandbox.ID},
		{name: "unknown project", username: sandboxShort + ".nosuch", wantErr: true},
		{name: "unknown sandbox id", username: "sbx_nosuchbox00000", wantErr: true},
		{name: "no dot and not sbx-prefixed", username: "plainword", wantErr: true},
		{name: "empty", username: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projectID, sandboxID, err := ResolveUsername(ctx, db, tt.username)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got projectID=%q sandboxID=%q", projectID, sandboxID)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if projectID != tt.wantProjectID || sandboxID != tt.wantSandboxID {
				t.Fatalf("got (%q, %q), want (%q, %q)", projectID, sandboxID, tt.wantProjectID, tt.wantSandboxID)
			}
		})
	}
}

func TestResolveUsernameSbxPrefixWinsEvenWithDot(t *testing.T) {
	ctx := context.Background()
	db := newRouteTestStore(t)
	acme := createRouteFixtureProject(t, db, "proj_acme00000000", "Acme")
	sandbox := createRouteFixtureSandbox(t, db, acme, "devbox")

	// A username that starts with the sandbox ID prefix is always parsed as a
	// bare sandbox ID/prefix, even if a dotted form could also be imagined.
	if _, _, err := ResolveUsername(ctx, db, sandbox.ID+".whatever"); err == nil {
		t.Fatalf("expected sbx_-prefixed username with a trailing dot to fail as a bare ID lookup, not fall back to dotted parsing")
	}
}

func newRouteTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return store.New(db.Write, db.Read)
}

func createRouteFixtureProject(t *testing.T, s *store.Store, id, name string) string {
	t.Helper()
	ctx := context.Background()
	if err := s.UpsertProject(ctx, &model.Project{ID: id, OwnerUserID: "user-1", Name: name}); err != nil {
		t.Fatalf("create project %s: %v", id, err)
	}
	return id
}

func createRouteFixtureSandbox(t *testing.T, s *store.Store, projectID, name string) *model.Sandbox {
	t.Helper()
	ctx := context.Background()
	sandboxID := idpkg.NewString(idpkg.PrefixSandbox)
	providerID := "prov-" + sandboxID
	if err := s.CreateSandboxProviderInstance(ctx, &model.SandboxProviderInstance{ID: providerID, ProjectID: projectID, Type: "docker", Name: providerID}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	poolID := "pool-" + sandboxID
	if err := s.CreatePool(ctx, &model.Pool{ID: poolID, ProjectID: projectID, PoolManifest: model.PoolManifest{Name: poolID, ProviderInstanceID: providerID}}); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	sandbox := &model.Sandbox{
		ID:              sandboxID,
		ProjectID:       projectID,
		PoolID:          poolID,
		CreatedByUserID: "user-1",
		Name:            name,
	}
	if err := s.CreateSandbox(ctx, sandbox); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	return sandbox
}
