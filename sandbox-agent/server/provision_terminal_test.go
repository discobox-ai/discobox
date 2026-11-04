package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/discobox/sandbox-agent/provision"
)

func TestTerminalReadsDistinguishPreparationFromUnknownTerminal(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	cfg := testConfigWithRunner(publicKey, &sandboxAgentFakeRunner{})
	cfg.Provision = provision.NewTracker("")
	router, err := NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provision.Status().Ready {
		t.Fatal("fixture must still be preparing")
	}
	token := signToken("project-1", "sandbox-1", "worker-1", ScopeExecRead)
	for _, suffix := range []string{"", "/screen"} {
		t.Run("primary"+suffix, func(t *testing.T) {
			resp := getInventory(t, router, "/api/projects/project-1/sandboxes/sandbox-1/execs/primary"+suffix, token)
			body := strings.ToLower(resp.Body.String())
			if resp.Code == http.StatusNotFound || !strings.Contains(body, "prepar") && !strings.Contains(body, "provision") {
				t.Errorf("terminal read while preparing = %d %s; want preparation guidance instead of missing exec", resp.Code, resp.Body.String())
			}
		})
		t.Run("unknown"+suffix, func(t *testing.T) {
			resp := getInventory(t, router, "/api/projects/project-1/sandboxes/sandbox-1/execs/exec-does-not-exist"+suffix, token)
			if resp.Code != http.StatusNotFound {
				t.Errorf("unknown terminal = %d %s; want 404", resp.Code, resp.Body.String())
			}
		})
	}
}

// A canceled command creation must leave no record to attach to. Preparation
// failure must be named, while a concrete unknown ID remains a missing exec.
func TestCommandCreationWhilePreparingIsCancelable(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	cfg := testConfigWithRunner(publicKey, &sandboxAgentFakeRunner{})
	cfg.Provision = provision.NewTracker("")
	router, err := NewRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	token := signToken("project-1", "sandbox-1", "worker-1", ScopeExecRead, ScopeExecWrite)
	ctx, cancel := context.WithCancel(t.Context())
	ctx, deadline := context.WithTimeout(ctx, time.Second)
	defer deadline()
	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/projects/project-1/sandboxes/sandbox-1/execs", strings.NewReader(`{"command":["true"]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() { defer close(done); router.ServeHTTP(resp, req) }()
	select {
	case <-done:
		t.Fatalf("creation returned before preparation or cancellation: %d %s", resp.Code, resp.Body.String())
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("creation ignored cancellation")
	}
	if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "preparing") {
		t.Fatalf("cancellation = %d %s", resp.Code, resp.Body.String())
	}
	list := getInventory(t, router, "/api/projects/project-1/sandboxes/sandbox-1/execs", token)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"execs":[]`) {
		t.Fatalf("canceled creation left an exec: %d %s", list.Code, list.Body.String())
	}
	cfg.Provision.Fail(errors.New("core installation failed"))
	for _, suffix := range []string{"", "/screen"} {
		failed := getInventory(t, router, "/api/projects/project-1/sandboxes/sandbox-1/execs/primary"+suffix, token)
		if failed.Code != http.StatusConflict || !strings.Contains(failed.Body.String(), "core installation failed") {
			t.Fatalf("failed preparation = %d %s", failed.Code, failed.Body.String())
		}
	}
}

func TestPrimaryReadsBeforeRecordAndShim(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	cfg := testConfigWithRunner(publicKey, &sandboxAgentFakeRunner{})
	built, err := newRouterAndManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	token := signToken("project-1", "sandbox-1", "worker-1", ScopeExecRead)
	// No provision gate remains, but boot has not yet made the first record.
	for _, suffix := range []string{"", "/screen"} {
		resp := getInventory(t, built.router, "/api/projects/project-1/sandboxes/sandbox-1/execs/primary"+suffix, token)
		if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "preparing") {
			t.Fatalf("before record: %d %s", resp.Code, resp.Body.String())
		}
	}
	e, err := built.execs.Create(t.Context(), execs.CreateRequest{Command: []string{"sh"}, TTY: true, Metadata: map[string]string{"primary": "true", "harnessId": "shell"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"primary", e.ID} {
		resp := getInventory(t, built.router, "/api/projects/project-1/sandboxes/sandbox-1/execs/"+id+"/screen", token)
		if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "preparing") {
			t.Fatalf("before shim: %d %s", resp.Code, resp.Body.String())
		}
	}
	resp := getInventory(t, built.router, "/api/projects/project-1/sandboxes/sandbox-1/execs/primary", token)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"status":"starting"`) {
		t.Fatalf("starting record: %d %s", resp.Code, resp.Body.String())
	}
}
