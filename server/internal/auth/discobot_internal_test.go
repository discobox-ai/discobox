package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/auth/discobot"
)

// asserted runs a request through authentication and authorization as the
// server wires them for discobot, and reports the status and, when the
// handler ran, the principal and path it saw.
func asserted(t *testing.T, key ed25519.PublicKey, method, path, assertion string) (int, Principal, string) {
	t.Helper()
	var seen Principal
	var seenPath string
	handler := Authentication(
		DiscobotAuthenticator{PublicKey: key},
		DefaultUserAuthenticator{UserID: "default-user"},
	)(Authorization(
		ProjectAuthorizer{},
		AuthenticatedAuthorizer{},
	)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = PrincipalFromContext(r.Context())
		seenPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})))
	request := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	if assertion != "" {
		request.Header.Set(discobot.AssertionHeader, assertion)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Code, seen, seenPath
}

func discobotKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return public, private
}

func sign(t *testing.T, private ed25519.PrivateKey, projectID string) string {
	t.Helper()
	token, err := discobot.Sign(private, discobot.Claims{UserID: "usr_priya", Name: "Priya", ProjectID: projectID, ID: "a1"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestADiscobotAssertionActsAsThePersonInTheirProject(t *testing.T) {
	public, private := discobotKey(t)
	status, principal, _ := asserted(t, public, http.MethodGet, "/projects/proj_acme/sandboxes", sign(t, private, "proj_acme"))
	if status != http.StatusNoContent {
		t.Fatalf("status = %d, want the request through", status)
	}
	if principal.Type != PrincipalTypeUser || principal.UserID != "usr_priya" || principal.ProjectID != "proj_acme" || !principal.Asserted() {
		t.Fatalf("principal = %#v", principal)
	}
}

func TestADiscobotAssertionResolvesDefaultToItsProject(t *testing.T) {
	public, private := discobotKey(t)
	status, _, path := asserted(t, public, http.MethodGet, "/projects/default/sandboxes", sign(t, private, "proj_acme"))
	if status != http.StatusNoContent || path != "/projects/proj_acme/sandboxes" {
		t.Fatalf("status = %d, path = %q, want proj_acme", status, path)
	}
}

func TestADiscobotAssertionReachesNoOtherProject(t *testing.T) {
	public, private := discobotKey(t)
	if status, _, _ := asserted(t, public, http.MethodGet, "/projects/proj_other/sandboxes", sign(t, private, "proj_acme")); status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for another project", status)
	}
	if status, _, _ := asserted(t, public, http.MethodGet, "/projects/proj_acme/sandboxes", sign(t, private, "")); status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for an assertion naming no project", status)
	}
}

func TestADiscobotAssertionReachesOnlySomeServerRoutes(t *testing.T) {
	public, private := discobotKey(t)
	token := sign(t, private, "")
	for path, want := range map[string]int{
		"/projects":            http.StatusNoContent,
		"/providers/catalog":   http.StatusNoContent,
		"/harness-definitions": http.StatusNoContent,
		"/shutdown":            http.StatusForbidden,
		"/peers":               http.StatusForbidden,
		"/api/pools/register":  http.StatusForbidden,
	} {
		if status, _, _ := asserted(t, public, http.MethodGet, path, token); status != want {
			t.Errorf("%s: status = %d, want %d", path, status, want)
		}
	}
}

// An assertion that does not verify is refused outright; falling through
// would answer it as the default user.
func TestABadDiscobotAssertionIsNeverTheDefaultUser(t *testing.T) {
	public, _ := discobotKey(t)
	_, otherPrivate := discobotKey(t)
	if status, _, _ := asserted(t, public, http.MethodGet, "/projects/proj_acme/sandboxes", sign(t, otherPrivate, "proj_acme")); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an assertion signed by another key", status)
	}
	_, private := discobotKey(t)
	if status, _, _ := asserted(t, nil, http.MethodGet, "/projects/proj_acme/sandboxes", sign(t, private, "proj_acme")); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 from a server not configured to trust discobot", status)
	}
}

func TestWithoutAnAssertionTheDefaultUserIsUnchanged(t *testing.T) {
	public, _ := discobotKey(t)
	authenticated := Authentication(DiscobotAuthenticator{PublicKey: public}, DefaultUserAuthenticator{UserID: "default-user"})
	var seen Principal
	handler := authenticated(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen, _ = PrincipalFromContext(r.Context())
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/projects", nil))
	if seen.UserID != "default-user" || seen.Asserted() {
		t.Fatalf("principal = %#v, want the default user", seen)
	}
}
