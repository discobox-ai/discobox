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
		"/agent-config-definitions",
		"/agent-config-definitions/example",
		"/api/workers/register",
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
	if !principal.HasScope("sandbox:read") || !principal.HasScope("sandbox:write") || !principal.HasScope("sandbox:http") || !principal.HasScope("terminal:read") || !principal.HasScope("terminal:write") {
		t.Fatalf("default principal scopes = %#v, want all scopes", principal.Scopes)
	}
}
