package proxy

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestControlAuthRequiresValidToken(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	auth, err := newControlAuthenticator(ControlConfig{
		TrustPublicKey: base64.StdEncoding.EncodeToString(publicKey),
		ProjectID:      "project-1",
		WorkerID:       "worker-1",
	})
	if err != nil {
		t.Fatalf("newControlAuthenticator() error = %v", err)
	}
	handler := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := ControlTokenClaimsFromContext(r.Context())
		if !ok {
			t.Fatal("missing control claims")
		}
		if claims.ProjectID != "project-1" || claims.WorkerID != "worker-1" {
			t.Fatalf("claims = %#v", claims)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/audit/http", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", rec.Code)
	}

	token, err := CreateControlToken(privateKey, ControlTokenClaims{ProjectID: "project-1", WorkerID: "worker-1", Scopes: []string{ScopeAuditRead}})
	if err != nil {
		t.Fatalf("CreateControlToken() error = %v", err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/audit/http", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("authorized status = %d body=%q", rec.Code, rec.Body.String())
	}
}

func TestControlAuthSandboxTokenRestrictsClientQuery(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	auth, err := newControlAuthenticator(ControlConfig{TrustPublicKey: base64.StdEncoding.EncodeToString(publicKey)})
	if err != nil {
		t.Fatalf("newControlAuthenticator() error = %v", err)
	}
	handler := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	token, err := CreateControlToken(privateKey, ControlTokenClaims{
		ProjectID: "project-1",
		WorkerID:  "worker-1",
		SandboxID: "sandbox-1",
		Scopes:    []string{ScopeAuditRead},
	})
	if err != nil {
		t.Fatalf("CreateControlToken() error = %v", err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/audit/http?client_id=sandbox-2", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("sandbox mismatch status = %d", rec.Code)
	}
}

// The mismatch check above is not the boundary: it only fires when the caller
// sends a client_id at all. A sandbox-scoped token that omits it must still be
// narrowed to its own sandbox, or it reads every sandbox's rows — and, through
// the artifact routes, their spooled bodies.
//
// Asserted on the request the handler receives, and over every route shape,
// because the narrowing is the middleware's: a route added later inherits it
// without knowing it exists.
func TestControlAuthSandboxTokenNarrowsEveryUnscopedRoute(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	auth, err := newControlAuthenticator(ControlConfig{
		TrustPublicKey: base64.StdEncoding.EncodeToString(publicKey),
		ProjectID:      "project-1",
		WorkerID:       "worker-1",
	})
	if err != nil {
		t.Fatalf("newControlAuthenticator() error = %v", err)
	}
	token, err := CreateControlToken(privateKey, ControlTokenClaims{
		ProjectID: "project-1",
		WorkerID:  "worker-1",
		SandboxID: "sandbox-1",
		Scopes:    []string{ScopeAuditRead},
	})
	if err != nil {
		t.Fatalf("CreateControlToken() error = %v", err)
	}
	for _, target := range []string{
		"/audit/http",
		"/audit/socks",
		"/audit/dns",
		"/audit/http?host=api.example.com",
		"/audit/http/7/request-body",
		"/audit/http/7/response-body",
		"/audit/http/7/stream",
	} {
		var got string
		handler := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.URL.Query().Get("client_id")
			w.WriteHeader(http.StatusNoContent)
		}))
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: status = %d, want 204", target, rec.Code)
		}
		if got != "sandbox-1" {
			t.Fatalf("%s: handler saw client_id = %q, want the token's own sandbox", target, got)
		}
	}
}

// Narrowing must not disturb the rest of the query it rewrites.
func TestControlAuthNarrowingPreservesOtherParameters(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	auth, err := newControlAuthenticator(ControlConfig{TrustPublicKey: base64.StdEncoding.EncodeToString(publicKey)})
	if err != nil {
		t.Fatalf("newControlAuthenticator() error = %v", err)
	}
	token, err := CreateControlToken(privateKey, ControlTokenClaims{SandboxID: "sandbox-1", Scopes: []string{ScopeAuditRead}})
	if err != nil {
		t.Fatalf("CreateControlToken() error = %v", err)
	}
	var query url.Values
	handler := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/audit/http?host=api.example.com&min_status=400&limit=5", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if query.Get("client_id") != "sandbox-1" {
		t.Fatalf("client_id = %q", query.Get("client_id"))
	}
	if query.Get("host") != "api.example.com" || query.Get("min_status") != "400" || query.Get("limit") != "5" {
		t.Fatalf("narrowing disturbed the query: %v", query)
	}
}

// The caller's request must not be mutated: r.WithContext is a shallow clone
// that still shares its URL.
func TestControlAuthNarrowingDoesNotMutateCallerRequest(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	auth, err := newControlAuthenticator(ControlConfig{TrustPublicKey: base64.StdEncoding.EncodeToString(publicKey)})
	if err != nil {
		t.Fatalf("newControlAuthenticator() error = %v", err)
	}
	token, err := CreateControlToken(privateKey, ControlTokenClaims{SandboxID: "sandbox-1", Scopes: []string{ScopeAuditRead}})
	if err != nil {
		t.Fatalf("CreateControlToken() error = %v", err)
	}
	handler := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/audit/http", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	original := req.URL
	originalQuery := req.URL.RawQuery
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if req.URL != original || req.URL.RawQuery != originalQuery {
		t.Fatalf("caller request URL was mutated: %q -> %q", originalQuery, req.URL.RawQuery)
	}
}

// A token with no sandbox_id is the pool-wide reader, and keeps whatever scope
// the caller asked for.
func TestControlAuthUnscopedTokenKeepsTheQueryClient(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	auth, err := newControlAuthenticator(ControlConfig{
		TrustPublicKey: base64.StdEncoding.EncodeToString(publicKey),
	})
	if err != nil {
		t.Fatalf("newControlAuthenticator() error = %v", err)
	}
	var got string
	handler := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("client_id")
		w.WriteHeader(http.StatusNoContent)
	}))
	token, err := CreateControlToken(privateKey, ControlTokenClaims{Scopes: []string{ScopeAuditRead}})
	if err != nil {
		t.Fatalf("CreateControlToken() error = %v", err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/audit/http?client_id=sandbox-9", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got != "sandbox-9" {
		t.Fatalf("client_id = %q, want the query's client", got)
	}
}

// The one control route that deletes takes a scope of its own, and a token
// that names the client it purges: a read token cannot delete, and a purge
// token cannot read or purge another sandbox (ADR 26-11-14-896 §5).
func TestControlAuthPurgeNeedsItsOwnScopeAndItsSandbox(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	auth, err := newControlAuthenticator(ControlConfig{TrustPublicKey: base64.StdEncoding.EncodeToString(publicKey)})
	if err != nil {
		t.Fatalf("newControlAuthenticator() error = %v", err)
	}
	handler := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		name    string
		method  string
		path    string
		sandbox string
		scopes  []string
		want    int
	}{
		{"purge token purges its sandbox", http.MethodDelete, "/audit/clients/sbx_1", "sbx_1", []string{ScopeAuditPurge}, http.StatusNoContent},
		{"read token cannot purge", http.MethodDelete, "/audit/clients/sbx_1", "sbx_1", []string{ScopeAuditRead}, http.StatusForbidden},
		{"purge token cannot purge another sandbox", http.MethodDelete, "/audit/clients/sbx_2", "sbx_1", []string{ScopeAuditPurge}, http.StatusForbidden},
		{"pool-wide purge token names no sandbox", http.MethodDelete, "/audit/clients/sbx_1", "", []string{ScopeAuditPurge}, http.StatusForbidden},
		{"purge token cannot read", http.MethodGet, "/audit/http", "sbx_1", []string{ScopeAuditPurge}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := CreateControlToken(privateKey, ControlTokenClaims{ProjectID: "project-1", WorkerID: "worker-1", SandboxID: tc.sandbox, Scopes: tc.scopes})
			if err != nil {
				t.Fatalf("CreateControlToken() error = %v", err)
			}
			req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
