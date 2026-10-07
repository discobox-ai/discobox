package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type testAuthorizer struct {
	ok    bool
	err   error
	calls *int
}

func (a testAuthorizer) Authorize(*http.Request) (bool, error) {
	(*a.calls)++
	return a.ok, a.err
}

func TestAuthorizationAllowsFirstAuthorizerThatMatches(t *testing.T) {
	firstCalls := 0
	secondCalls := 0
	handler := Authorization(
		testAuthorizer{ok: true, calls: &firstCalls},
		testAuthorizer{ok: true, calls: &secondCalls},
	)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1", nil))

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
	if firstCalls != 1 || secondCalls != 0 {
		t.Fatalf("calls = first %d second %d, want first 1 second 0", firstCalls, secondCalls)
	}
}

func TestAuthorizationTriesNextAuthorizerWhenOneDoesNotApply(t *testing.T) {
	firstCalls := 0
	secondCalls := 0
	handler := Authorization(
		testAuthorizer{ok: false, calls: &firstCalls},
		testAuthorizer{ok: true, calls: &secondCalls},
	)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1", nil))

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("calls = first %d second %d, want first 1 second 1", firstCalls, secondCalls)
	}
}

func TestAuthorizationStopsOnAuthorizerError(t *testing.T) {
	firstCalls := 0
	secondCalls := 0
	handler := Authorization(
		testAuthorizer{err: errors.New("denied"), calls: &firstCalls},
		testAuthorizer{ok: true, calls: &secondCalls},
	)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1", nil))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
	if firstCalls != 1 || secondCalls != 0 {
		t.Fatalf("calls = first %d second %d, want first 1 second 0", firstCalls, secondCalls)
	}
}

func TestAuthenticatedAuthorizerAllowsOnlyExplicitPaths(t *testing.T) {
	principal := Principal{Type: PrincipalTypeUser, UserID: "user-1"}
	for _, path := range []string{
		"/harness-definitions",
		"/harness-definitions/example",
		"/api/pools/register",
		"/projects",
		"/providers/catalog",
		"/shutdown",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
			req = req.WithContext(WithPrincipal(req.Context(), principal))
			ok, err := (AuthenticatedAuthorizer{}).Authorize(req)
			if err != nil {
				t.Fatalf("authorize: %v", err)
			}
			if !ok {
				t.Fatalf("authorized = false, want true")
			}
		})
	}
}

func TestAuthenticatedAuthorizerDoesNotAuthorizeUnlistedPaths(t *testing.T) {
	principal := Principal{Type: PrincipalTypeUser, UserID: "user-1"}
	for _, path := range []string{
		"/projects/project-1",
		"/projects/project-1/sandboxes",
		"/api/workers/worker-1/status",
		"/providers",
		"/providers/catalog/provider-1",
		"/unknown",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
			req = req.WithContext(WithPrincipal(req.Context(), principal))
			ok, err := (AuthenticatedAuthorizer{}).Authorize(req)
			if err != nil {
				t.Fatalf("authorize: %v", err)
			}
			if ok {
				t.Fatalf("authorized = true, want false")
			}
		})
	}
}

func TestDefaultUserAuthenticatorGrantsAllScopes(t *testing.T) {
	principal, ok, err := (DefaultUserAuthenticator{UserID: "user-1"}).Authenticate(httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/projects", nil))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if !ok {
		t.Fatal("authenticate ok = false, want true")
	}
	if principal.Type != PrincipalTypeUser || principal.UserID != "user-1" {
		t.Fatalf("principal = %#v", principal)
	}
	if !principal.HasScope("sandbox:read") || !principal.HasScope("sandbox:write") || !principal.HasScope("sandbox:http") || !principal.HasScope("terminal:read") || !principal.HasScope("terminal:write") || !principal.HasScope("exec:read") || !principal.HasScope("exec:write") {
		t.Fatalf("default principal scopes = %#v, want all scopes", principal.Scopes)
	}
}

// SSH's only client is the CLI, so /ssh/connect is for a user principal that
// discobot did not assert, and for no pool agent or sandbox.
func TestSSHConnectAuthorizerAdmitsOnlyTheCLIsUser(t *testing.T) {
	for _, tc := range []struct {
		name      string
		principal Principal
		want      bool
	}{
		{"the default user", Principal{Type: PrincipalTypeUser, UserID: "user-default", Scopes: []string{ScopeAll}}, true},
		{"a person discobot asserted", Principal{Type: PrincipalTypeUser, UserID: "usr_priya", Issuer: "discobot"}, false},
		{"a pool agent", Principal{Type: PrincipalTypePool, PoolID: "pool-1"}, false},
		{"a sandbox", Principal{Type: PrincipalTypeSandbox, SandboxID: "sbx-1"}, false},
		{"a pool registering", Principal{Type: PrincipalTypePoolBootstrap}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(WithPrincipal(context.Background(), tc.principal), http.MethodGet, SSHConnectPath, nil)
			ok, err := (SSHConnectAuthorizer{}).Authorize(req)
			if ok != tc.want || (err == nil) != tc.want {
				t.Fatalf("Authorize = %v, %v; want admitted %v", ok, err, tc.want)
			}
		})
	}
	other := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/projects", nil)
	if ok, err := (SSHConnectAuthorizer{}).Authorize(other); ok || err != nil {
		t.Fatalf("Authorize(/projects) = %v, %v; want it not to apply", ok, err)
	}
}

// A starting pool agent reaches registration with no credential of its own;
// the bootstrap token in the body is checked by the pools service.
func TestPoolBootstrapAuthenticatorAppliesOnlyToRegistration(t *testing.T) {
	register := httptest.NewRequestWithContext(context.Background(), http.MethodPost, PoolRegisterPath, nil)
	if principal, ok, err := (PoolBootstrapAuthenticator{}).Authenticate(register); !ok || err != nil || principal.Type != PrincipalTypePoolBootstrap {
		t.Fatalf("Authenticate(register) = %#v, %v, %v", principal, ok, err)
	}
	other := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/projects", nil)
	if _, ok, err := (PoolBootstrapAuthenticator{}).Authenticate(other); ok || err != nil {
		t.Fatalf("Authenticate(/projects) = %v, %v; want it not to apply", ok, err)
	}
}
