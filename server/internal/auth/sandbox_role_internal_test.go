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

// roleStore holds the discoboxes the owned routes are checked against — one
// the lead created, and one a person did — and secret requests filed by them
// and by no discobox at all.
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
	// A request from each: the lead's worker, a person's discobox, a discobox
	// that is gone, and none at all — a person's own ask.
	for _, req := range []*model.SecretRequest{
		{ID: "sreq-worker", SandboxID: "sbx-worker"},
		{ID: "sreq-persons", SandboxID: "sbx-persons"},
		{ID: "sreq-gone", SandboxID: "sbx-gone"},
		{ID: "sreq-person", RequestedBy: "user-1"},
	} {
		req.ProjectID, req.Type, req.Status = "proj-1", "token", model.SecretRequestStatusPending
		if req.RequestedBy == "" {
			req.RequestedBy = "agent:" + req.SandboxID
		}
		if err := st.CreateSecretRequest(ctx, req); err != nil {
			t.Fatalf("create secret request %s: %v", req.ID, err)
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

// A discobox reads and types into the terminals only of discoboxes it created,
// one call at a time; attaching and starting or ending an exec stay out (ADR
// 26-10-01-397).
func TestSandboxRoleDrivesOnlyItsOwnDiscoboxesTerminals(t *testing.T) {
	authorizer := SandboxRoleAuthorizer{Store: roleStore(t)}
	lead := Principal{Type: PrincipalTypeSandbox, SandboxID: "sbx-lead", ProjectID: "proj-1", UserID: "user-1"}
	for _, tc := range []struct {
		name, method, path string
		want               int
	}{
		{"list its worker's terminals", http.MethodGet, "/projects/default/sandboxes/sbx-worker/execs", http.StatusOK},
		{"read its worker's screen", http.MethodGet, "/api/projects/default/sandboxes/sbx-worker/execs/primary/screen", http.StatusOK},
		{"wait on its worker's terminal", http.MethodPost, "/projects/proj-1/sandboxes/sbx-worker/execs/primary/wait", http.StatusOK},
		{"type into its worker's terminal", http.MethodPost, "/projects/default/sandboxes/sbx-worker/execs/exec-1/input", http.StatusOK},
		{"read a person's discobox's screen", http.MethodGet, "/projects/default/sandboxes/sbx-persons/execs/primary/screen", http.StatusForbidden},
		{"type into a person's discobox", http.MethodPost, "/projects/default/sandboxes/sbx-persons/execs/primary/input", http.StatusForbidden},
		{"list a person's discobox's terminals", http.MethodGet, "/projects/default/sandboxes/sbx-persons/execs", http.StatusForbidden},
		{"read nothing's screen", http.MethodGet, "/projects/default/sandboxes/sbx-none/execs/primary/screen", http.StatusNotFound},
		{"attach to its worker", http.MethodGet, "/projects/default/sandboxes/sbx-worker/execs/primary/attach", http.StatusForbidden},
		{"start an exec in its worker", http.MethodPost, "/projects/default/sandboxes/sbx-worker/execs", http.StatusForbidden},
		{"end its worker's exec", http.MethodDelete, "/projects/default/sandboxes/sbx-worker/execs/exec-1", http.StatusForbidden},
		{"read its worker's logs", http.MethodGet, "/projects/default/sandboxes/sbx-worker/execs/exec-1/logs", http.StatusForbidden},
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

// A discobox starts, stops, and restarts only discoboxes it created — not a
// person's, and not itself; archiving, purging, repairing, and upgrading stay
// out (ADR 26-10-02-478).
func TestSandboxRolePowersOnlyWhatItCreated(t *testing.T) {
	authorizer := SandboxRoleAuthorizer{Store: roleStore(t)}
	lead := Principal{Type: PrincipalTypeSandbox, SandboxID: "sbx-lead", ProjectID: "proj-1", UserID: "user-1"}
	worker := Principal{Type: PrincipalTypeSandbox, SandboxID: "sbx-worker", ProjectID: "proj-1", UserID: "user-1"}
	for _, tc := range []struct {
		name         string
		caller       Principal
		method, path string
		want         int
	}{
		{"start its worker", lead, http.MethodPost, "/projects/default/sandboxes/sbx-worker/start", http.StatusOK},
		{"stop its worker", lead, http.MethodPost, "/api/projects/default/sandboxes/sbx-worker/stop", http.StatusOK},
		{"restart its worker", lead, http.MethodPost, "/projects/proj-1/sandboxes/sbx-worker/restart", http.StatusOK},
		{"start a person's discobox", lead, http.MethodPost, "/projects/default/sandboxes/sbx-persons/start", http.StatusForbidden},
		{"stop a person's discobox", lead, http.MethodPost, "/projects/default/sandboxes/sbx-persons/stop", http.StatusForbidden},
		{"restart a person's discobox", lead, http.MethodPost, "/projects/default/sandboxes/sbx-persons/restart", http.StatusForbidden},
		{"stop itself", worker, http.MethodPost, "/projects/default/sandboxes/sbx-worker/stop", http.StatusForbidden},
		{"start nothing", lead, http.MethodPost, "/projects/default/sandboxes/sbx-none/start", http.StatusNotFound},
		{"start its worker in another project", lead, http.MethodPost, "/projects/proj-2/sandboxes/sbx-worker/start", http.StatusForbidden},
		{"archive its worker", lead, http.MethodDelete, "/projects/default/sandboxes/sbx-worker", http.StatusForbidden},
		{"purge its worker", lead, http.MethodPost, "/projects/default/sandboxes/sbx-worker/purge", http.StatusForbidden},
		{"repair its worker", lead, http.MethodPost, "/projects/default/sandboxes/sbx-worker/repair", http.StatusForbidden},
		{"upgrade its worker", lead, http.MethodPost, "/projects/default/sandboxes/sbx-worker/upgrade", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(WithPrincipal(context.Background(), tc.caller), tc.method, tc.path, nil)
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

// A discobox changes the meta only of discoboxes it created — not a person's,
// and not its own through the API; renaming one stays out (ADR 26-10-08-447).
func TestSandboxRoleTagsOnlyWhatItCreated(t *testing.T) {
	authorizer := SandboxRoleAuthorizer{Store: roleStore(t)}
	lead := Principal{Type: PrincipalTypeSandbox, SandboxID: "sbx-lead", ProjectID: "proj-1", UserID: "user-1"}
	worker := Principal{Type: PrincipalTypeSandbox, SandboxID: "sbx-worker", ProjectID: "proj-1", UserID: "user-1"}
	for _, tc := range []struct {
		name         string
		caller       Principal
		method, path string
		want         int
	}{
		{"tag its worker", lead, http.MethodPatch, "/projects/default/sandboxes/sbx-worker/meta", http.StatusOK},
		{"tag its worker by its project", lead, http.MethodPatch, "/api/projects/proj-1/sandboxes/sbx-worker/meta", http.StatusOK},
		{"tag a person's discobox", lead, http.MethodPatch, "/projects/default/sandboxes/sbx-persons/meta", http.StatusForbidden},
		{"tag itself", worker, http.MethodPatch, "/projects/default/sandboxes/sbx-worker/meta", http.StatusForbidden},
		{"tag nothing", lead, http.MethodPatch, "/projects/default/sandboxes/sbx-none/meta", http.StatusNotFound},
		{"tag its worker in another project", lead, http.MethodPatch, "/projects/proj-2/sandboxes/sbx-worker/meta", http.StatusForbidden},
		{"rename its worker", lead, http.MethodPatch, "/projects/default/sandboxes/sbx-worker", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(WithPrincipal(context.Background(), tc.caller), tc.method, tc.path, nil)
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

// A discobox reads and answers only the requests of discoboxes it created
// (ADR 26-09-30-782 §2). A request from a discobox a person made, from one that
// is gone, or from no discobox at all is a person's to answer.
func TestSandboxRoleAnswersOnlyItsOwnDiscoboxesRequests(t *testing.T) {
	authorizer := SandboxRoleAuthorizer{Store: roleStore(t)}
	lead := Principal{Type: PrincipalTypeSandbox, SandboxID: "sbx-lead", ProjectID: "proj-1", UserID: "user-1"}
	for _, tc := range []struct {
		name, method, path string
		want               int
	}{
		{"read its worker's request", http.MethodGet, "/projects/default/secret-requests/sreq-worker", http.StatusOK},
		{"approve its worker's request", http.MethodPost, "/projects/default/secret-requests/sreq-worker/approve", http.StatusOK},
		{"deny its worker's request", http.MethodPost, "/projects/proj-1/secret-requests/sreq-worker/deny", http.StatusOK},
		{"list requests", http.MethodGet, "/projects/default/secret-requests", http.StatusOK},
		{"read a person's discobox's request", http.MethodGet, "/projects/default/secret-requests/sreq-persons", http.StatusForbidden},
		{"approve a person's discobox's request", http.MethodPost, "/projects/default/secret-requests/sreq-persons/approve", http.StatusForbidden},
		{"approve a gone discobox's request", http.MethodPost, "/projects/default/secret-requests/sreq-gone/approve", http.StatusForbidden},
		{"approve a person's own request", http.MethodPost, "/projects/default/secret-requests/sreq-person/approve", http.StatusForbidden},
		{"approve nothing", http.MethodPost, "/projects/default/secret-requests/sreq-none/approve", http.StatusNotFound},
		// The listing is filtered to what it was delegated where it is served
		// (ADR 26-09-30-782 §3); the route itself is open to it.
		{"list secrets", http.MethodGet, "/projects/default/secrets", http.StatusOK},
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

// The listing a discobox reads is filtered to the requests it owns, since the
// route that serves it cannot filter what it answers with.
func TestADiscoboxListsOnlyTheRequestsItOwns(t *testing.T) {
	st := roleStore(t)
	owned, err := st.ListSecretRequests(context.Background(), "proj-1", "", store.OwnedBy("sbx-lead"))
	if err != nil {
		t.Fatalf("list owned: %v", err)
	}
	if len(owned) != 1 || owned[0].ID != "sreq-worker" {
		t.Fatalf("owned = %+v, want only its worker's request", owned)
	}
	all, err := st.ListSecretRequests(context.Background(), "proj-1", "")
	if err != nil || len(all) != 4 {
		t.Fatalf("all = %d requests, %v; want every one for a person", len(all), err)
	}
}
