package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/discobox/sandbox-agent/provision"
)

func TestTerminalReadsDistinguishPreparationFromUnknownTerminal(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	cfg := testConfigWithRunner(t, publicKey, &sandboxAgentFakeRunner{})
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
	cfg := testConfigWithRunner(t, publicKey, &sandboxAgentFakeRunner{})
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
	cfg := testConfigWithRunner(t, publicKey, &sandboxAgentFakeRunner{})
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

// A cold install can exceed the terminal startup budget while its command
// can remain silent. Exercise the real runner's progress signal and
// the HTTP command path, rather than just the tracker's wait in isolation.
func TestExecRequestsSurviveLongProvisioning(t *testing.T) {
	for _, tc := range []struct{ name, path, body string }{
		{"command", "", `{"command":["true"]}`},
		{"shell", "", `{"shell":true}`},
		{"terminal", "", `{}`},
		{"primary", "/primary/start", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			publicKey, signToken := sandboxAgentTestSigner(t)
			root := t.TempDir()
			featureDir := filepath.Join(root, "feature")
			if err := os.MkdirAll(featureDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(featureDir, "install.sh"), []byte("#!/bin/sh\n"), 0755); err != nil {
				t.Fatal(err)
			}
			synctest.Test(t, func(t *testing.T) {
				cfg := testConfigWithRunner(t, publicKey, &sandboxAgentFakeRunner{})
				cfg.Provision = provision.NewTracker("")
				if tc.name == "primary" {
					cfg.Installer = reachedInstaller{}
				}
				router, err := NewRouter(cfg)
				if err != nil {
					t.Fatal(err)
				}
				token := signToken("project-1", "sandbox-1", "worker-1", ScopeExecRead, ScopeExecWrite)
				runner := provision.Runner{
					Root:     root,
					Records:  provision.Records{Dir: filepath.Join(root, "records")},
					Platform: provision.Platform{OS: "linux", Arch: "amd64", ID: "debian"},
					Tracker:  cfg.Provision,
					Command: func(_ context.Context, _ io.Writer, _ string, _ []string, _ string, _ ...string) error {
						time.Sleep(4 * time.Minute)
						return nil
					},
				}
				done := make(chan error, 1)
				go func() {
					_, err := runner.Provision(t.Context(), []provisionspec.Feature{{Name: "core", Dir: featureDir, Spec: provisionspec.Spec{Steps: []provisionspec.Step{{Name: "install", Run: "install.sh"}}}}}, provision.Options{})
					done <- err
				}()
				defer func() {
					if err := <-done; err != nil {
						t.Error(err)
					}
				}()
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/projects/project-1/sandboxes/sandbox-1/execs"+tc.path, strings.NewReader(tc.body))
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				resp := httptest.NewRecorder()
				started := time.Now()
				router.ServeHTTP(resp, req)
				if tc.name == "primary" {
					// The fake unit manager has no shim. Stop at install to prove the
					// original start request reached launch after the long provision.
					if resp.Code != http.StatusInternalServerError || !strings.Contains(resp.Body.String(), "reached terminal installer") {
						t.Fatalf("primary launch = %d %s", resp.Code, resp.Body.String())
					}
				} else if resp.Code != http.StatusCreated && resp.Code != http.StatusOK {
					t.Fatalf("creation = %d %s", resp.Code, resp.Body.String())
				}
				if time.Since(started) < 4*time.Minute {
					t.Fatalf("command created before provisioning ended: %s", time.Since(started))
				}
			})
		})
	}
}

type reachedInstaller struct{ sandboxAgentNoopInstaller }

func (reachedInstaller) EnsureInstalled(context.Context, config.Harness, string, map[string]string) error {
	return errors.New("reached terminal installer")
}
