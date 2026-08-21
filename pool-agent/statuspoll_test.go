package poolagent

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// statusPollTestRuntime wraps MemorySandboxRuntime (which already implements
// every other sandboxruntime.Runtime method) and routes HTTPBaseURL to a
// per-sandbox fake HTTP server, or an error for a sandbox configured to be
// unreachable.
type statusPollTestRuntime struct {
	*sandboxruntime.MemorySandboxRuntime
	servers       map[string]*httptest.Server
	failSandboxID string
}

func (r *statusPollTestRuntime) HTTPBaseURL(_ context.Context, sandboxID string, _ int) (*url.URL, error) {
	if sandboxID == r.failSandboxID {
		return nil, fmt.Errorf("simulated unreachable sandbox %s", sandboxID)
	}
	srv, ok := r.servers[sandboxID]
	if !ok {
		return nil, fmt.Errorf("no test server for sandbox %s", sandboxID)
	}
	return url.Parse(srv.URL)
}

type fakeSandboxAgentStatusClient struct {
	mu       sync.Mutex
	reported []SandboxAgentStatusEntry
	mints    int
}

func (c *fakeSandboxAgentStatusClient) MintSandboxAgentStatusTokens(_ context.Context, req MintSandboxAgentStatusTokensRequest) (*MintSandboxAgentStatusTokensResponse, error) {
	c.mu.Lock()
	c.mints++
	c.mu.Unlock()
	resp := &MintSandboxAgentStatusTokensResponse{}
	for _, id := range req.SandboxIDs {
		resp.Tokens = append(resp.Tokens, SandboxAgentStatusToken{
			SandboxID: id, Token: "token-" + id, ExpiresAt: time.Now().Add(15 * time.Minute),
		})
	}
	return resp, nil
}

func (c *fakeSandboxAgentStatusClient) ReportSandboxAgentStatus(_ context.Context, req SandboxAgentStatusReportRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reported = append(c.reported, req.Sandboxes...)
	return nil
}

func statusPollTestRegistration(t *testing.T) *Registration {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &Registration{PrivateKey: priv}
}

func statusOKHandler(observedAt string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sources":[],"sessions":[],"observedAt":"` + observedAt + `"}`))
	}
}

// TestSandboxAgentStatusPollerIsolatesPerSandboxErrors confirms one
// unreachable sandbox does not block or fail the tick for the others, and
// that only the sandboxes successfully polled are pushed.
func TestSandboxAgentStatusPollerIsolatesPerSandboxErrors(t *testing.T) {
	good1 := httptest.NewServer(statusOKHandler("2026-01-01T00:00:00Z"))
	defer good1.Close()
	good2 := httptest.NewServer(statusOKHandler("2026-01-01T00:00:01Z"))
	defer good2.Close()

	runtime := &statusPollTestRuntime{
		MemorySandboxRuntime: sandboxruntime.NewMemorySandboxRuntime(),
		servers: map[string]*httptest.Server{
			"sandbox-good-1": good1,
			"sandbox-good-2": good2,
		},
		failSandboxID: "sandbox-bad",
	}
	ctx := context.Background()
	for _, id := range []string{"sandbox-good-1", "sandbox-bad", "sandbox-good-2"} {
		if _, err := runtime.CreateSandbox(ctx, &workerapimodel.PoolSandboxCreateRequest{SandboxId: id}); err != nil {
			t.Fatalf("seed sandbox %s: %v", id, err)
		}
	}

	client := &fakeSandboxAgentStatusClient{}
	poller := &sandboxAgentStatusPoller{
		logger:       slog.New(slog.DiscardHandler),
		bootstrap:    Bootstrap{ControlPlaneURL: "http://control-plane.invalid", ProjectID: "project-1", PoolID: "pool-1"},
		registration: statusPollTestRegistration(t),
		runtime:      runtime,
		client:       client,
		tokens:       map[string]cachedSandboxAgentToken{},
	}

	poller.tick(ctx)

	client.mu.Lock()
	reported := append([]SandboxAgentStatusEntry(nil), client.reported...)
	client.mu.Unlock()

	if len(reported) != 2 {
		t.Fatalf("reported %d entries, want 2 (the bad sandbox must be skipped, not fail the batch): %+v", len(reported), reported)
	}
	seen := map[string]bool{}
	for _, entry := range reported {
		seen[entry.SandboxID] = true
	}
	if !seen["sandbox-good-1"] || !seen["sandbox-good-2"] {
		t.Fatalf("reported sandboxes = %v, want sandbox-good-1 and sandbox-good-2", seen)
	}
	if seen["sandbox-bad"] {
		t.Fatal("sandbox-bad was reported, want it skipped")
	}
}

// TestSandboxAgentStatusPollerCachesTokensAcrossTicks confirms a cached,
// unexpired token is reused rather than re-minted on every tick.
func TestSandboxAgentStatusPollerCachesTokensAcrossTicks(t *testing.T) {
	good := httptest.NewServer(statusOKHandler("2026-01-01T00:00:00Z"))
	defer good.Close()

	runtime := &statusPollTestRuntime{
		MemorySandboxRuntime: sandboxruntime.NewMemorySandboxRuntime(),
		servers:              map[string]*httptest.Server{"sandbox-1": good},
	}
	ctx := context.Background()
	if _, err := runtime.CreateSandbox(ctx, &workerapimodel.PoolSandboxCreateRequest{SandboxId: "sandbox-1"}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}

	client := &fakeSandboxAgentStatusClient{}
	poller := &sandboxAgentStatusPoller{
		logger:       slog.New(slog.DiscardHandler),
		bootstrap:    Bootstrap{ControlPlaneURL: "http://control-plane.invalid", ProjectID: "project-1", PoolID: "pool-1"},
		registration: statusPollTestRegistration(t),
		runtime:      runtime,
		client:       client,
		tokens:       map[string]cachedSandboxAgentToken{},
	}

	poller.tick(ctx)
	poller.tick(ctx)
	poller.tick(ctx)

	client.mu.Lock()
	mints := client.mints
	reported := len(client.reported)
	client.mu.Unlock()

	if mints != 1 {
		t.Fatalf("mint calls = %d, want 1 (token should be cached across ticks)", mints)
	}
	if reported != 3 {
		t.Fatalf("reported entries across 3 ticks = %d, want 3", reported)
	}
}

// TestSandboxAgentStatusPollerRefreshesExpiringToken confirms a token within
// the refresh margin of expiry is re-minted rather than reused.
func TestSandboxAgentStatusPollerRefreshesExpiringToken(t *testing.T) {
	good := httptest.NewServer(statusOKHandler("2026-01-01T00:00:00Z"))
	defer good.Close()

	runtime := &statusPollTestRuntime{
		MemorySandboxRuntime: sandboxruntime.NewMemorySandboxRuntime(),
		servers:              map[string]*httptest.Server{"sandbox-1": good},
	}
	ctx := context.Background()
	if _, err := runtime.CreateSandbox(ctx, &workerapimodel.PoolSandboxCreateRequest{SandboxId: "sandbox-1"}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}

	client := &fakeSandboxAgentStatusClient{}
	poller := &sandboxAgentStatusPoller{
		logger:       slog.New(slog.DiscardHandler),
		bootstrap:    Bootstrap{ControlPlaneURL: "http://control-plane.invalid", ProjectID: "project-1", PoolID: "pool-1"},
		registration: statusPollTestRegistration(t),
		runtime:      runtime,
		client:       client,
		tokens: map[string]cachedSandboxAgentToken{
			// Already within the refresh margin of expiry.
			"sandbox-1": {token: "stale-token", expiresAt: time.Now().Add(1 * time.Minute)},
		},
	}

	poller.tick(ctx)

	client.mu.Lock()
	mints := client.mints
	client.mu.Unlock()
	if mints != 1 {
		t.Fatalf("mint calls = %d, want 1 (near-expiry token should be refreshed)", mints)
	}
}
