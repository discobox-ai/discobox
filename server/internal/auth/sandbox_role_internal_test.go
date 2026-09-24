package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// roleStore holds the discoboxes the source delivery routes are checked
// against: one the lead created, and one a person did.
func roleStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	st := store.New(db.Write, db.Read)
	if err := st.UpsertProject(ctx, &model.Project{ID: "proj-1", OwnerUserID: "user-1", Name: "proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := st.CreateSandboxProviderInstance(ctx, &model.SandboxProviderInstance{ID: "prov-1", ProjectID: "proj-1", Type: "docker", Name: "prov-1"}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if err := st.CreatePool(ctx, &model.Pool{ID: "pool-1", ProjectID: "proj-1", PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: "prov-1"}}); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	lead := "sbx-lead"
	for _, sandbox := range []*model.Sandbox{
		{ID: "sbx-worker", Name: "worker", CreatedBySandboxID: &lead},
		{ID: "sbx-persons", Name: "persons"},
	} {
		sandbox.ProjectID, sandbox.PoolID, sandbox.CreatedByUserID = "proj-1", "pool-1", "user-1"
		if err := st.CreateSandbox(ctx, sandbox); err != nil {
			t.Fatalf("create sandbox %s: %v", sandbox.ID, err)
		}
	}
	return st
}

// A sandbox delivers source only into a discobox it created, only by pushing,
// and only in its own project (ADR 26-09-24-630 §2).
func TestSandboxRoleDeliversSourceOnlyToWhatItCreated(t *testing.T) {
	authorizer := SandboxRoleAuthorizer{Store: roleStore(t)}
	lead := Principal{Type: PrincipalTypeSandbox, SandboxID: "sbx-lead", ProjectID: "proj-1", UserID: "user-1"}
	for _, tc := range []struct {
		name, method, path string
		want               int
	}{
		{"push refs of its worker", http.MethodGet, "/projects/default/sandboxes/sbx-worker/git-origins/primary.git/info/refs?service=git-receive-pack", http.StatusOK},
		{"push into its worker", http.MethodPost, "/projects/proj-1/sandboxes/sbx-worker/git-origins/primary.git/git-receive-pack", http.StatusOK},
		{"complete its worker's push", http.MethodPost, "/projects/default/sandboxes/sbx-worker/complete-source-push", http.StatusOK},
		{"fetch refs of its worker", http.MethodGet, "/projects/default/sandboxes/sbx-worker/git-origins/primary.git/info/refs?service=git-upload-pack", http.StatusForbidden},
		{"fetch from its worker", http.MethodPost, "/projects/default/sandboxes/sbx-worker/git-origins/primary.git/git-upload-pack", http.StatusForbidden},
		{"push into a person's discobox", http.MethodPost, "/projects/default/sandboxes/sbx-persons/git-origins/primary.git/git-receive-pack", http.StatusForbidden},
		{"complete a person's push", http.MethodPost, "/projects/default/sandboxes/sbx-persons/complete-source-push", http.StatusForbidden},
		{"push into nothing", http.MethodPost, "/projects/default/sandboxes/sbx-none/complete-source-push", http.StatusNotFound},
		{"push into its worker's work repository", http.MethodPost, "/projects/default/sandboxes/sbx-worker/git-repositories/primary.git/git-receive-pack", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(WithPrincipal(context.Background(), lead), tc.method, tc.path, nil)
			ok, err := authorizer.Authorize(r)
			got := http.StatusOK
			if err != nil {
				var status interface{ StatusCode() int }
				if !errors.As(err, &status) {
					t.Fatalf("error %v carries no status", err)
				}
				got = status.StatusCode()
			} else if !ok {
				t.Fatal("the role stepped aside for a sandbox's call; it must answer every one")
			}
			if got != tc.want {
				t.Fatalf("status = %d (%v), want %d", got, err, tc.want)
			}
		})
	}
}
