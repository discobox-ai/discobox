package poolagent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	apimodel "github.com/discobox-ai/discobox/api/model"
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

// The counters this agent differences are decoded out of a payload it otherwise
// relays untouched. This pins the one direction that is not covered by the
// relay itself: sandbox-agent's own status JSON must decode into the counters
// the resource reporter reads, and the relayed bytes must survive unchanged.
func TestPollDecodedStatusCarriesResourceCountersAndRelaysVerbatim(t *testing.T) {
	payload := []byte(`{
		"sources": [],
		"sessions": [],
		"ports": [],
		"observedAt": "2026-08-27T12:00:00Z",
		"resources": {
			"observedAt": "2026-08-27T11:59:59Z",
			"source": "cgroup",
			"cpu": {"usageUsec": 8204113000, "userUsec": 6000000000, "systemUsec": 2204113000},
			"memory": {"currentBytes": 6442450944, "virtualBytes": 12884901888, "residentBytes": 5368709120},
			"processCount": 42,
			"processes": [
				{"pid": 100, "command": "node", "cmdline": "node vitest",
				 "startTicks": 5512, "cpuUsec": 3400000, "virtualBytes": 8589934592, "residentBytes": 4294967296}
			]
		}
	}`)

	var decoded struct {
		ObservedAt time.Time                           `json:"observedAt"`
		Resources  *apimodel.SandboxAgentResourceUsage `json:"resources"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode sandbox-agent status: %v", err)
	}
	if decoded.Resources == nil {
		t.Fatal("resources did not decode")
	}
	if decoded.Resources.CPU.UsageUsec != 8_204_113_000 {
		t.Errorf("usageUsec = %d", decoded.Resources.CPU.UsageUsec)
	}
	if decoded.Resources.Memory.VirtualBytes != 12_884_901_888 || decoded.Resources.Memory.ResidentBytes != 5_368_709_120 {
		t.Errorf("memory = %+v", decoded.Resources.Memory)
	}
	if len(decoded.Resources.Processes) != 1 || decoded.Resources.Processes[0].StartTicks != 5512 {
		t.Errorf("processes = %+v", decoded.Resources.Processes)
	}

	// The entry the poller builds relays the payload byte for byte; only
	// observedAt and resources are read out of it.
	entry := SandboxAgentStatusEntry{
		SandboxID:  "sbx_a",
		Status:     payload,
		ObservedAt: decoded.ObservedAt,
		Resources:  decoded.Resources,
	}
	relayed, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	// Compacted, because encoding/json compacts a json.RawMessage on the way
	// out — but never re-serialized from a parsed structure, so no field the
	// pool agent does not know about can be dropped or reordered.
	if !bytes.Contains(relayed, []byte(`"processCount":42`)) {
		t.Error("the relayed status lost a field the pool agent does not model")
	}
	if !bytes.Contains(relayed, []byte(`"cmdline":"node vitest"`)) {
		t.Error("the relayed status lost nested content")
	}
	if bytes.Contains(relayed, []byte(`"Resources"`)) {
		t.Error("the decoded counters leaked onto the status channel; they belong to the resource report")
	}
}
