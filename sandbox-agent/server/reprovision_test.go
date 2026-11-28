package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"github.com/discobox-ai/discobox/sandbox-agent/provision"
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

func TestStatusIncludesScheduledReprovision(t *testing.T) {
	tracker := provision.NewTracker("")
	tracker.EnableAutomaticRetry(t.Context(), func(provisionspec.Status) {
		t.Error("retry started during the status request")
	})
	tracker.Fail(errors.New("fetch failed"))
	defer tracker.Rerun()
	router, token, _ := runtimeConfigRouterWithPoolConfig(t, func(cfg *Config) {
		cfg.Provision = tracker
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/status", nil)
	req.Header.Set("Authorization", "Bearer "+token(ScopeStatusRead))
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	var body struct {
		Provision provisionspec.Status `json:"provision"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := tracker.Status().Retry
	got := body.Provision.Retry
	if resp.Code != http.StatusOK || got == nil || want == nil || got.Attempt != want.Attempt || got.Of != want.Of || !got.At.Equal(want.At.Truncate(time.Second)) {
		t.Fatalf("status response %d: retry %+v, want %+v", resp.Code, got, want)
	}
}

// A running step's progress is carried in the status report as the agent
// holds it, its zero counts left out.
func TestProvisionStatusCarriesProgress(t *testing.T) {
	at := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	out := sandboxAgentProvisionStatus(provisionspec.Status{State: provisionspec.StateProvisioning, Features: map[string]provisionspec.FeatureStatus{
		"node": {State: provisionspec.StateProvisioning, Step: "install", Progress: &provisionspec.Progress{Detail: "installing nodejs", Percent: 62, Received: 4096, ReceivedAt: at}},
		"go":   {State: provisionspec.StatePending},
	}})
	progress, ok := out.Features["node"].Progress.Get()
	if !ok || progress.Detail.Or("") != "installing nodejs" || progress.Percent.Or(0) != 62 || progress.Received.Or(0) != 4096 || !progress.ReceivedAt.Or(time.Time{}).Equal(at) || progress.Done.IsSet() || progress.Total.IsSet() {
		t.Fatalf("progress = %+v, %v", progress, ok)
	}
	if out.Features["go"].Progress.IsSet() {
		t.Fatal("a pending feature carried progress")
	}
	// A box that cannot count what it receives reports no count.
	out = sandboxAgentProvisionStatus(provisionspec.Status{State: provisionspec.StateProvisioning, Features: map[string]provisionspec.FeatureStatus{
		"node": {State: provisionspec.StateProvisioning, Step: "install", Progress: &provisionspec.Progress{Detail: "installing"}},
	}})
	if progress := out.Features["node"].Progress.Or(sandboxapi.SandboxAgentProvisionProgress{}); progress.Received.IsSet() || progress.ReceivedAt.IsSet() {
		t.Fatalf("progress = %+v, want no received count", progress)
	}
}
