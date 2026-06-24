package proxy

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
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
