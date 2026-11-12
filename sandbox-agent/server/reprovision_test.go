package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// A reprovision acts in the box as starting an exec does: exec:write lets it
// through, and the agent answers 202 with the document once the run has
// started (ADR 26-11-11-560 §5).
func TestReprovisionRunsWithExecWrite(t *testing.T) {
	asked := 0
	router, token, _ := runtimeConfigRouterWithPoolConfig(t, func(cfg *Config) {
		cfg.Reprovision = func() provisionspec.Status {
			asked++
			return provisionspec.Status{State: provisionspec.StateProvisioning, Ready: true, Features: map[string]provisionspec.FeatureStatus{
				"go": {State: provisionspec.StatePending, Async: true},
			}}
		}
	})
	serve := func(tok, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		return resp
	}
	const route = "/api/projects/project-1/sandboxes/sandbox-1/reprovision"
	if resp := serve(token(ScopeExecRead), route); resp.Code != http.StatusForbidden && resp.Code != http.StatusUnauthorized {
		t.Fatalf("with exec:read = %d, want refused", resp.Code)
	}
	// The generated router decodes an escaped letter, so an escaped spelling
	// needs the same scope.
	if resp := serve(token(ScopeExecRead), "/api/projects/project-1/sandboxes/sandbox-1/%72eprovision"); resp.Code != http.StatusForbidden && resp.Code != http.StatusUnauthorized {
		t.Fatalf("escaped route with exec:read = %d, want refused", resp.Code)
	}
	if asked != 0 {
		t.Fatal("a refused request ran")
	}
	resp := serve(token(ScopeExecWrite), route)
	if resp.Code != http.StatusAccepted || asked != 1 {
		t.Fatalf("with exec:write = %d %s, asked %d times", resp.Code, resp.Body.String(), asked)
	}
	var status provisionspec.Status
	if err := json.Unmarshal(resp.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.State != provisionspec.StateProvisioning || status.Features["go"].State != provisionspec.StatePending {
		t.Fatalf("answered %+v", status)
	}
}

// A box with no provisioning has nothing to run again.
func TestReprovisionWithNoProvisioningConflicts(t *testing.T) {
	router, token, _ := runtimeConfigRouterWithPoolConfig(t, func(*Config) {})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/projects/project-1/sandboxes/sandbox-1/reprovision", nil)
	req.Header.Set("Authorization", "Bearer "+token(ScopeExecWrite))
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusConflict {
		t.Fatalf("status = %d %s", resp.Code, resp.Body.String())
	}
}
