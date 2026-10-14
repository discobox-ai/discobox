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

// roleStore holds the discoboxes the role's routes are checked against — one
// the lead created, one a person did, and the project's judge — and secret
// requests filed by them and by no discobox at all.
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
	if err := st.UpsertProject(ctx, &model.Project{ID: "proj-1", OwnerUserID: "user-1", Name: "proj-1", JudgeSandboxID: "sbx-judge"}); err != nil {
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
		{ID: "sbx-judge", Name: "judge"},
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

// roleCase is one call the sandbox role answers, and the status it answers
// with: 200 for a call it lets through.
type roleCase struct {
	name         string
	caller       Principal
	method, path string
	want         int
}

var (
	roleLead   = Principal{Type: PrincipalTypeSandbox, SandboxID: "sbx-lead", ProjectID: "proj-1", UserID: "user-1"}
	roleWorker = Principal{Type: PrincipalTypeSandbox, SandboxID: "sbx-worker", ProjectID: "proj-1", UserID: "user-1"}
)

func checkRole(t *testing.T, cases []roleCase) {
	t.Helper()
	authorizer := SandboxRoleAuthorizer{Store: roleStore(t)}
	for _, tc := range cases {
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

// A sandbox delivers source into any discobox in its project, only by
// pushing (ADR 26-09-24-630 §2, ADR 26-10-14-320 §1).
func TestSandboxRoleDeliversSourceToAnyDiscoboxInItsProject(t *testing.T) {
	checkRole(t, []roleCase{
		{"push refs of its worker", roleLead, http.MethodGet, "/projects/default/sandboxes/sbx-worker/git-origins/primary.git/info/refs?service=git-receive-pack", http.StatusOK},
		{"push into its worker", roleLead, http.MethodPost, "/projects/proj-1/sandboxes/sbx-worker/git-origins/primary.git/git-receive-pack", http.StatusOK},
		{"complete its worker's push", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-worker/complete-source-push", http.StatusOK},
		{"pin its worker's source", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-worker/git-origins/primary.git/pin", http.StatusOK},
		{"pin a person's source", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-persons/git-origins/primary.git/pin", http.StatusOK},
		{"push into a person's discobox", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-persons/git-origins/primary.git/git-receive-pack", http.StatusOK},
		{"complete a person's push", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-persons/complete-source-push", http.StatusOK},
		{"push into its creator", roleWorker, http.MethodPost, "/projects/default/sandboxes/sbx-persons/git-origins/primary.git/git-receive-pack", http.StatusOK},
		// What a pool's cache remembers of a remote is a project member's to
		// fetch, never a sandbox's (ADR 26-10-10-545 §7).
		{"fetch refs of its worker's cache", roleLead, http.MethodGet, "/projects/default/sandboxes/sbx-worker/git-caches/primary.git/info/refs?service=git-upload-pack", http.StatusForbidden},
		{"fetch from its worker's cache", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-worker/git-caches/primary.git/git-upload-pack", http.StatusForbidden},
		{"fetch refs of its worker", roleLead, http.MethodGet, "/projects/default/sandboxes/sbx-worker/git-origins/primary.git/info/refs?service=git-upload-pack", http.StatusForbidden},
		{"fetch from its worker", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-worker/git-origins/primary.git/git-upload-pack", http.StatusForbidden},
		{"push into nothing", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-none/complete-source-push", http.StatusNotFound},
		{"push into its worker's work repository", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-worker/git-repositories/primary.git/git-receive-pack", http.StatusForbidden},
		{"push into another project", roleLead, http.MethodPost, "/projects/proj-2/sandboxes/sbx-worker/complete-source-push", http.StatusForbidden},
	})
}

// A discobox reads and types into the terminals of any discobox in its
// project, one call at a time, and reads an exec's record, transcript and
// events; attaching and starting or ending an exec stay out (ADR
// 26-10-01-397, ADR 26-10-14-320 §§1, 3).
func TestSandboxRoleDrivesTheTerminalsOfAnyDiscoboxInItsProject(t *testing.T) {
	checkRole(t, []roleCase{
		{"list its worker's terminals", roleLead, http.MethodGet, "/projects/default/sandboxes/sbx-worker/execs", http.StatusOK},
		{"read its worker's screen", roleLead, http.MethodGet, "/api/projects/default/sandboxes/sbx-worker/execs/primary/screen", http.StatusOK},
		{"wait on its worker's terminal", roleLead, http.MethodPost, "/projects/proj-1/sandboxes/sbx-worker/execs/primary/wait", http.StatusOK},
		{"type into its worker's terminal", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-worker/execs/exec-1/input", http.StatusOK},
		{"read a person's discobox's screen", roleLead, http.MethodGet, "/projects/default/sandboxes/sbx-persons/execs/primary/screen", http.StatusOK},
		{"type into a person's discobox", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-persons/execs/primary/input", http.StatusOK},
		{"list a person's discobox's terminals", roleLead, http.MethodGet, "/projects/default/sandboxes/sbx-persons/execs", http.StatusOK},
		{"read an exec", roleLead, http.MethodGet, "/api/projects/default/sandboxes/sbx-persons/execs/exec-1", http.StatusOK},
		{"read an exec's logs", roleLead, http.MethodGet, "/api/projects/default/sandboxes/sbx-persons/execs/exec-1/logs", http.StatusOK},
		{"read an exec's events", roleLead, http.MethodGet, "/api/projects/default/sandboxes/sbx-worker/execs/exec-1/events", http.StatusOK},
		{"read nothing's screen", roleLead, http.MethodGet, "/projects/default/sandboxes/sbx-none/execs/primary/screen", http.StatusNotFound},
		{"attach to its worker", roleLead, http.MethodGet, "/projects/default/sandboxes/sbx-worker/execs/primary/attach", http.StatusForbidden},
		{"start an exec in its worker", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-worker/execs", http.StatusForbidden},
		{"start a created exec", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-worker/execs/exec-1/start", http.StatusForbidden},
		{"end its worker's exec", roleLead, http.MethodDelete, "/projects/default/sandboxes/sbx-worker/execs/exec-1", http.StatusForbidden},
		{"read an exec's resources", roleLead, http.MethodGet, "/api/projects/default/sandboxes/sbx-worker/execs/exec-1/resources", http.StatusForbidden},
	})
}

// A discobox starts, stops, restarts, repairs and upgrades any discobox in
// its project, itself included; archiving and purging stay out (ADR
// 26-10-02-478, ADR 26-10-14-320 §§1, 3).
func TestSandboxRolePowersAnyDiscoboxInItsProject(t *testing.T) {
	checkRole(t, []roleCase{
		{"start its worker", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-worker/start", http.StatusOK},
		{"stop its worker", roleLead, http.MethodPost, "/api/projects/default/sandboxes/sbx-worker/stop", http.StatusOK},
		{"restart its worker", roleLead, http.MethodPost, "/projects/proj-1/sandboxes/sbx-worker/restart", http.StatusOK},
		{"start a person's discobox", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-persons/start", http.StatusOK},
		{"stop a person's discobox", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-persons/stop", http.StatusOK},
		{"restart a person's discobox", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-persons/restart", http.StatusOK},
		{"stop itself", roleWorker, http.MethodPost, "/projects/default/sandboxes/sbx-worker/stop", http.StatusOK},
		{"repair its worker", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-worker/repair", http.StatusOK},
		{"upgrade a person's discobox", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-persons/upgrade", http.StatusOK},
		{"start nothing", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-none/start", http.StatusNotFound},
		{"start its worker in another project", roleLead, http.MethodPost, "/projects/proj-2/sandboxes/sbx-worker/start", http.StatusForbidden},
		{"archive its worker", roleLead, http.MethodDelete, "/projects/default/sandboxes/sbx-worker", http.StatusForbidden},
		{"purge its worker", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-worker/purge", http.StatusForbidden},
		{"unarchive its worker", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-worker/unarchive", http.StatusForbidden},
	})
}

// A discobox changes the meta of any discobox in its project; renaming one
// stays out (ADR 26-10-08-447, ADR 26-10-14-320 §1).
func TestSandboxRoleTagsAnyDiscoboxInItsProject(t *testing.T) {
	checkRole(t, []roleCase{
		{"tag its worker", roleLead, http.MethodPatch, "/projects/default/sandboxes/sbx-worker/meta", http.StatusOK},
		{"tag its worker by its project", roleLead, http.MethodPatch, "/api/projects/proj-1/sandboxes/sbx-worker/meta", http.StatusOK},
		{"tag a person's discobox", roleLead, http.MethodPatch, "/projects/default/sandboxes/sbx-persons/meta", http.StatusOK},
		{"tag itself", roleWorker, http.MethodPatch, "/projects/default/sandboxes/sbx-worker/meta", http.StatusOK},
		{"tag nothing", roleLead, http.MethodPatch, "/projects/default/sandboxes/sbx-none/meta", http.StatusNotFound},
		{"tag its worker in another project", roleLead, http.MethodPatch, "/projects/proj-2/sandboxes/sbx-worker/meta", http.StatusForbidden},
		{"rename its worker", roleLead, http.MethodPatch, "/projects/default/sandboxes/sbx-worker", http.StatusForbidden},
	})
}

// A discobox reads, starts and stops the services of any discobox in its
// project, and reaches any of its ports over HTTP with any method (ADR
// 26-10-14-320 §3). A dot segment never reaches a port: where it resolves is
// not what the route named.
func TestSandboxRoleReachesTheServicesAndPortsOfAnyDiscoboxInItsProject(t *testing.T) {
	checkRole(t, []roleCase{
		{"list services", roleLead, http.MethodGet, "/api/projects/default/sandboxes/sbx-persons/services", http.StatusOK},
		{"read a service", roleLead, http.MethodGet, "/api/projects/default/sandboxes/sbx-persons/services/web", http.StatusOK},
		{"read a service's logs", roleLead, http.MethodGet, "/api/projects/default/sandboxes/sbx-worker/services/web/logs", http.StatusOK},
		{"start a service", roleLead, http.MethodPost, "/api/projects/default/sandboxes/sbx-worker/services/web/start", http.StatusOK},
		{"stop a service", roleLead, http.MethodPost, "/api/projects/default/sandboxes/sbx-persons/services/web/stop", http.StatusOK},
		{"restart a service", roleLead, http.MethodPost, "/api/projects/proj-1/sandboxes/sbx-worker/services/web/restart", http.StatusOK},
		{"delete a service", roleLead, http.MethodDelete, "/api/projects/default/sandboxes/sbx-worker/services/web", http.StatusForbidden},
		{"get a port's root", roleLead, http.MethodGet, "/projects/default/sandboxes/sbx-persons/http/8080", http.StatusOK},
		{"get a port's root with a slash", roleLead, http.MethodGet, "/projects/default/sandboxes/sbx-persons/http/8080/", http.StatusOK},
		{"post to a port", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-worker/http/3000/api/items", http.StatusOK},
		{"delete on a port", roleLead, http.MethodDelete, "/projects/default/sandboxes/sbx-worker/http/3000/api/items/1", http.StatusOK},
		{"a dot segment on a port", roleLead, http.MethodGet, "/projects/default/sandboxes/sbx-worker/http/3000/a/../b", http.StatusForbidden},
		{"climb out of a port", roleLead, http.MethodGet, "/projects/default/sandboxes/sbx-worker/http/3000/../../execs", http.StatusForbidden},
		{"an empty segment on a port", roleLead, http.MethodGet, "/projects/default/sandboxes/sbx-worker/http/3000//a", http.StatusForbidden},
		{"a port of nothing", roleLead, http.MethodGet, "/projects/default/sandboxes/sbx-none/http/8080", http.StatusNotFound},
		{"a port in another project", roleLead, http.MethodGet, "/projects/proj-2/sandboxes/sbx-worker/http/8080", http.StatusForbidden},
		{"tunnel TCP", roleLead, http.MethodGet, "/projects/default/sandboxes/sbx-worker/tcp/attach", http.StatusForbidden},
	})
}

// No route of the role reaches the project's judge, whoever created it and
// whatever a use says (ADR 26-10-14-320 §4).
func TestSandboxRoleNeverReachesTheJudge(t *testing.T) {
	checkRole(t, []roleCase{
		{"read it", roleLead, http.MethodGet, "/projects/default/sandboxes/sbx-judge", http.StatusForbidden},
		{"push into it", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-judge/git-origins/primary.git/git-receive-pack", http.StatusForbidden},
		{"read its screen", roleLead, http.MethodGet, "/api/projects/default/sandboxes/sbx-judge/execs/primary/screen", http.StatusForbidden},
		{"type into it", roleLead, http.MethodPost, "/api/projects/default/sandboxes/sbx-judge/execs/primary/input", http.StatusForbidden},
		{"read its logs", roleLead, http.MethodGet, "/api/projects/default/sandboxes/sbx-judge/execs/exec-1/logs", http.StatusForbidden},
		{"restart it", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-judge/restart", http.StatusForbidden},
		{"repair it", roleLead, http.MethodPost, "/projects/default/sandboxes/sbx-judge/repair", http.StatusForbidden},
		{"upgrade it", roleLead, http.MethodPost, "/projects/proj-1/sandboxes/sbx-judge/upgrade", http.StatusForbidden},
		{"tag it", roleLead, http.MethodPatch, "/projects/default/sandboxes/sbx-judge/meta", http.StatusForbidden},
		{"stop its service", roleLead, http.MethodPost, "/api/projects/default/sandboxes/sbx-judge/services/web/stop", http.StatusForbidden},
		{"reach its port", roleLead, http.MethodGet, "/projects/default/sandboxes/sbx-judge/http/8080", http.StatusForbidden},
		{"list the project's discoboxes", roleLead, http.MethodGet, "/projects/default/sandboxes", http.StatusOK},
	})
}

// A discobox reads and answers only the requests of discoboxes it created
// (ADR 26-09-30-782 §2). A request from a discobox a person made, from one that
// is gone, or from no discobox at all is a person's to answer.
func TestSandboxRoleAnswersOnlyItsOwnDiscoboxesRequests(t *testing.T) {
	checkRole(t, []roleCase{
		{"read its worker's request", roleLead, http.MethodGet, "/projects/default/secret-requests/sreq-worker", http.StatusOK},
		{"approve its worker's request", roleLead, http.MethodPost, "/projects/default/secret-requests/sreq-worker/approve", http.StatusOK},
		{"deny its worker's request", roleLead, http.MethodPost, "/projects/proj-1/secret-requests/sreq-worker/deny", http.StatusOK},
		{"list requests", roleLead, http.MethodGet, "/projects/default/secret-requests", http.StatusOK},
		{"list the project's skills sources", roleLead, http.MethodGet, "/projects/default/skills-sources", http.StatusOK},
		{"add a skills source", roleLead, http.MethodPost, "/projects/default/skills-sources", http.StatusForbidden},
		{"remove a skills source", roleLead, http.MethodDelete, "/projects/default/skills-sources/skills-1", http.StatusForbidden},
		{"read a person's discobox's request", roleLead, http.MethodGet, "/projects/default/secret-requests/sreq-persons", http.StatusForbidden},
		{"approve a person's discobox's request", roleLead, http.MethodPost, "/projects/default/secret-requests/sreq-persons/approve", http.StatusForbidden},
		{"approve a gone discobox's request", roleLead, http.MethodPost, "/projects/default/secret-requests/sreq-gone/approve", http.StatusForbidden},
		{"approve a person's own request", roleLead, http.MethodPost, "/projects/default/secret-requests/sreq-person/approve", http.StatusForbidden},
		// A discobox is never its own creator, so it never answers its own
		// request: a delegation it holds is not a use (ADR 26-10-14-320 §2).
		{"approve its own request", roleWorker, http.MethodPost, "/projects/default/secret-requests/sreq-worker/approve", http.StatusForbidden},
		{"approve nothing", roleLead, http.MethodPost, "/projects/default/secret-requests/sreq-none/approve", http.StatusNotFound},
		// The listing is filtered to what it was delegated where it is served
		// (ADR 26-09-30-782 §3); the route itself is open to it.
		{"list secrets", roleLead, http.MethodGet, "/projects/default/secrets", http.StatusOK},
	})
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
