package poolruntime

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/x/gormdb"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	poolagentserver "github.com/discobox-ai/discobox/pool-agent/server"
	"github.com/discobox-ai/discobox/proxy"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

// newHTTPAuditRuntimeProvider stands up the whole read path ADR 0130 §4
// describes, in process: a pool proxy whose audit database holds two rows and
// whose control API trusts one key, and a real pool-agent router relaying to it
// with a client signing with that key. The token the agent accepts carries
// exactly the given scopes.
func newHTTPAuditRuntimeProvider(t *testing.T, scopes ...string) *testRuntimeProvider {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	prepared, err := proxy.PrepareCertificates(proxy.PrepareOptions{
		Dir: filepath.Join(dir, "certs"), ProxyURL: "https://127.0.0.1:0", ServerHosts: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatalf("prepare proxy certificates: %v", err)
	}
	dsn := filepath.Join(dir, "audit.db")
	proxyServer, err := proxy.NewServer(ctx, proxy.Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   dsn,
		Recording:     proxy.RecordingConfig{Enabled: true, QueueSize: 16},
		Control: proxy.ControlConfig{
			TrustPublicKey: base64.StdEncoding.EncodeToString(publicKey),
			ProjectID:      "project-1",
			WorkerID:       "pool-1",
		},
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	t.Cleanup(func() { _ = proxyServer.Close() })
	seedHTTPAuditRows(t, dsn)
	control := httptest.NewServer(proxyServer.ControlHandler())
	t.Cleanup(control.Close)

	controlPlaneKey, poolToken := newPoolAgentTestAuth(t, "project-1", "pool-1", scopes...)
	router, err := poolagentserver.NewRouter(poolagentserver.Config{
		Identity:              poolagentserver.Identity{ProjectID: "project-1", PoolID: "pool-1"},
		Runtime:               sandboxruntime.NewMemorySandboxRuntime(),
		Audit:                 proxy.NewControlClient(control.URL, privateKey, "project-1", "pool-1", nil),
		ControlPlanePublicKey: controlPlaneKey,
	})
	if err != nil {
		t.Fatalf("new pool-agent router: %v", err)
	}
	agent := httptest.NewServer(router)
	t.Cleanup(agent.Close)
	return &testRuntimeProvider{baseURL: agent.URL, client: agent.Client(), token: poolToken}
}

// seedHTTPAuditRows writes rows into the audit database the proxy migrated.
// The proxy's recorder is its own; what this test reads is everything after
// the rows exist.
func seedHTTPAuditRows(t *testing.T, dsn string) {
	t.Helper()
	pools, err := gormdb.Open(gormdb.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open audit db: %v", err)
	}
	defer pools.Close()
	older := time.Now().UTC().Add(-2 * time.Minute)
	newer := time.Now().UTC().Add(-time.Minute)
	// sandbox-1's response body is spooled where the proxy looks for it by
	// default, beside its database.
	bodyDir := filepath.Join(filepath.Dir(dsn), "proxy-bodies")
	if err := os.MkdirAll(bodyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bodyDir, "response-1"), []byte(`{"login":"octocat"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		createdAt    time.Time
		client       string
		url, host    string
		uses         string
		status       int
		responseBody string
	}{
		{older, "sandbox-1", "https://api.github.com/user", "api.github.com", "use_abc", 200, "response-1"},
		{newer, "sandbox-2", "https://example.com/", "example.com", "", 404, ""},
	} {
		if err := pools.Write.Exec(
			`INSERT INTO http_exchanges (created_at, enqueued_at, written_at, client_id, method, url, host, status, duration_millis, swapped_use_ids, response_body_file, response_body_format)
			 VALUES (?, ?, ?, ?, 'GET', ?, ?, ?, 12, ?, ?, 'raw')`,
			row.createdAt, row.createdAt, row.createdAt, row.client, row.url, row.host, row.status, row.uses, row.responseBody,
		).Error; err != nil {
			t.Fatalf("seed audit row: %v", err)
		}
	}
}

func TestPoolProviderListHTTPAuditReadsTheProxyThroughTheAgent(t *testing.T) {
	runtimeProvider := newHTTPAuditRuntimeProvider(t, poolagentserver.ScopeAuditRead)
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)
	ctx := context.Background()

	all, err := provider.ListHTTPAudit(ctx, activePool("pool-1"), sandbox.HTTPAuditQuery{})
	if err != nil {
		t.Fatalf("list http audit: %v", err)
	}
	if len(all) != 2 || all[0].SandboxID != "sandbox-2" || all[1].SandboxID != "sandbox-1" {
		t.Fatalf("exchanges = %+v, want both, newest first", all)
	}
	if !reflect.DeepEqual(all[1].SwappedUseIDs, []string{"use_abc"}) || all[1].Host != "api.github.com" || all[1].Status != 200 || all[1].DurationMillis != 12 {
		t.Fatalf("sandbox-1's exchange = %+v, want its fields carried through three hops", all[1])
	}
	if all[0].SwappedUseIDs == nil || len(all[0].SwappedUseIDs) != 0 {
		t.Fatalf("an exchange that spent nothing has uses %#v, want an empty list", all[0].SwappedUseIDs)
	}

	// The join the whole trail is for: one use ID, the one request that spent it.
	joined, err := provider.ListHTTPAudit(ctx, activePool("pool-1"), sandbox.HTTPAuditQuery{UseID: "use_abc"})
	if err != nil {
		t.Fatalf("list by use: %v", err)
	}
	if len(joined) != 1 || joined[0].SandboxID != "sandbox-1" {
		t.Fatalf("exchanges for use_abc = %+v", joined)
	}

	scoped, err := provider.ListHTTPAudit(ctx, activePool("pool-1"), sandbox.HTTPAuditQuery{SandboxID: "sandbox-2"})
	if err != nil {
		t.Fatalf("list by sandbox: %v", err)
	}
	if len(scoped) != 1 || scoped[0].SandboxID != "sandbox-2" {
		t.Fatalf("exchanges for sandbox-2 = %+v", scoped)
	}

	// A follower reads forward; the status bound travels all three hops.
	forward, err := provider.ListHTTPAudit(ctx, activePool("pool-1"), sandbox.HTTPAuditQuery{Ascending: true})
	if err != nil {
		t.Fatalf("list forward: %v", err)
	}
	if len(forward) != 2 || forward[0].SandboxID != "sandbox-1" {
		t.Fatalf("exchanges read forward = %+v, want oldest first", forward)
	}
	// The cursor reaches the proxy's own read, three hops down: after the
	// older row's id, only the newer row is left.
	cursored, err := provider.ListHTTPAudit(ctx, activePool("pool-1"), sandbox.HTTPAuditQuery{AfterID: forward[0].ID})
	if err != nil {
		t.Fatalf("list after a cursor: %v", err)
	}
	if len(cursored) != 1 || cursored[0].SandboxID != "sandbox-2" {
		t.Fatalf("exchanges after the first row = %+v", cursored)
	}
	if rows, err := provider.ListHTTPAudit(ctx, activePool("pool-1"), sandbox.HTTPAuditQuery{AfterID: cursored[0].ID}); err != nil || len(rows) != 0 {
		t.Fatalf("exchanges after the last row = %+v, %v; want nothing re-read", rows, err)
	}

	failed, err := provider.ListHTTPAudit(ctx, activePool("pool-1"), sandbox.HTTPAuditQuery{MinStatus: 400})
	if err != nil {
		t.Fatalf("list by status: %v", err)
	}
	if len(failed) != 1 || failed[0].Status != 404 {
		t.Fatalf("exchanges with status >= 400 = %+v", failed)
	}
}

// One exchange read in full carries the fields a list leaves out, and is
// narrowed to the sandbox the read names.
func TestPoolProviderGetsOneExchangeInFull(t *testing.T) {
	runtimeProvider := newHTTPAuditRuntimeProvider(t, poolagentserver.ScopeAuditRead)
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)
	ctx := context.Background()

	rows, err := provider.ListHTTPAudit(ctx, activePool("pool-1"), sandbox.HTTPAuditQuery{SandboxID: "sandbox-1"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("list sandbox-1 = %+v, %v", rows, err)
	}
	detail, err := provider.GetHTTPAudit(ctx, activePool("pool-1"), "sandbox-1", rows[0].ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if detail.ID != rows[0].ID || detail.URL != rows[0].URL || detail.Host != "api.github.com" {
		t.Fatalf("detail = %+v, want the exchange the list named", detail)
	}
	// What the summary does not carry: when the recorder wrote the row, and
	// that its response body can be read.
	if detail.WrittenAt.IsZero() || !detail.ResponseBodyRecorded || detail.ResponseBodyFormat != "raw" {
		t.Fatalf("detail = %+v, want the recorder's own fields", detail)
	}
	if _, err := provider.GetHTTPAudit(ctx, activePool("pool-1"), "sandbox-2", rows[0].ID); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("get scoped to another sandbox = %v, want not found", err)
	}
	if _, err := provider.GetHTTPAudit(ctx, activePool("pool-1"), "", 999999); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("get of an exchange that does not exist = %v, want not found", err)
	}
}

// A recorded body comes back through the agent as it was spooled, and a scope
// to another sandbox cannot reach it.
func TestPoolProviderOpensARecordedBodyThroughTheAgent(t *testing.T) {
	runtimeProvider := newHTTPAuditRuntimeProvider(t, poolagentserver.ScopeAuditRead)
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)
	ctx := context.Background()

	rows, err := provider.ListHTTPAudit(ctx, activePool("pool-1"), sandbox.HTTPAuditQuery{SandboxID: "sandbox-1"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("list sandbox-1 = %+v, %v", rows, err)
	}
	opened, err := provider.OpenHTTPAuditArtifact(ctx, activePool("pool-1"), "sandbox-1", rows[0].ID, "response-body")
	if err != nil {
		t.Fatalf("open response body: %v", err)
	}
	body, err := io.ReadAll(opened.Body)
	_ = opened.Body.Close()
	if err != nil || string(body) != `{"login":"octocat"}` || opened.Format != "raw" {
		t.Fatalf("response body = %q (format %q), %v", body, opened.Format, err)
	}

	if _, err := provider.OpenHTTPAuditArtifact(ctx, activePool("pool-1"), "sandbox-2", rows[0].ID, "response-body"); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("open sandbox-1's body scoped to sandbox-2 = %v, want not found", err)
	}
	if _, err := provider.OpenHTTPAuditArtifact(ctx, activePool("pool-1"), "", rows[0].ID, "request-body"); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("open a body that was never recorded = %v, want not found", err)
	}
}

// Reading every sandbox's traffic is not a sandbox read.
func TestPoolAgentHTTPAuditRequiresItsOwnScope(t *testing.T) {
	runtimeProvider := newHTTPAuditRuntimeProvider(t, poolagentserver.ScopeSandboxRead, poolagentserver.ScopeSandboxWrite)
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)

	_, err := provider.ListHTTPAudit(context.Background(), activePool("pool-1"), sandbox.HTTPAuditQuery{})
	if err == nil || !strings.Contains(err.Error(), "403") && !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("list http audit with sandbox scopes = %v, want forbidden", err)
	}
}

// The sandbox a read names goes into the token, not only the query, so the
// agent can narrow by what the control plane signed.
func TestPoolAgentClientHTTPAuditMintsASandboxScopedAuditToken(t *testing.T) {
	runtimeProvider := newHTTPAuditRuntimeProvider(t, poolagentserver.ScopeAuditRead)
	runtimeProvider.staticToken = true
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)

	_, _ = provider.ListHTTPAudit(context.Background(), activePool("pool-1"), sandbox.HTTPAuditQuery{SandboxID: "sandbox-1"})
	if len(manager.agentTokenClaims) == 0 {
		t.Fatal("list http audit minted no pool-agent token")
	}
	claims := manager.agentTokenClaims[0]
	if claims.ProjectID != "project-1" || claims.PoolID != "pool-1" || claims.SandboxID != "sandbox-1" || !reflect.DeepEqual(claims.Scopes, []string{poolagentauth.ScopeAuditRead}) {
		t.Fatalf("agent token claims = %#v", claims)
	}
}

func TestPoolAgentWithoutTheAuditRouteIsUnsupported(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(old.Close)
	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	runtimeProvider.baseURL = old.URL
	runtimeProvider.client = old.Client()
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)

	_, err := provider.ListHTTPAudit(context.Background(), activePool("pool-1"), sandbox.HTTPAuditQuery{})
	if !errors.Is(err, sandbox.ErrPoolAgentUnsupported) {
		t.Fatalf("list http audit against an agent without the route = %v, want ErrPoolAgentUnsupported", err)
	}
}

// An audit read is a read. When the pool agent cannot be reached it reports
// that and stops; it does not reconcile the pool back into existence the way an
// operation that needs the pool running does.
func TestPoolProviderHTTPAuditDoesNotReconcileAnUnreachablePool(t *testing.T) {
	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	runtimeProvider.unreachable = sandbox.ErrPoolNotReachable
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)

	_, err := provider.ListHTTPAudit(context.Background(), activePool("pool-1"), sandbox.HTTPAuditQuery{})
	if !errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Fatalf("list http audit against an unreachable pool = %v, want ErrPoolNotReachable", err)
	}
	if manager.scheduledReconciles != 0 {
		t.Fatalf("an audit read scheduled %d reconciles of the pool it read", manager.scheduledReconciles)
	}
	if runtimeProvider.acquireCalls != 1 {
		t.Fatalf("acquired %d clients, want one attempt and no retry", runtimeProvider.acquireCalls)
	}
}

// An agent too old for the hand-wired recording route answers the router's
// text/plain 404. Read as "no such recording" that reports a pool which
// recorded the body as one that recorded nothing (ADR 0130); it has to reach
// the caller as the pool being unreadable.
func TestPoolProviderSeparatesAMissingRouteFromAMissingRecording(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		body        string
		wantOldMiss bool
	}{
		{name: "router 404", contentType: "text/plain; charset=utf-8", body: "404 page not found\n", wantOldMiss: true},
		{name: "agent 404", contentType: "application/problem+json", body: `{"status":404,"title":"Not Found","detail":"recorded no response-body"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(agent.Close)
			runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
			runtimeProvider.baseURL = agent.URL
			runtimeProvider.client = agent.Client()
			manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
			provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)

			_, err := provider.OpenHTTPAuditArtifact(context.Background(), activePool("pool-1"), "sandbox-1", 42, "response-body")
			if tc.wantOldMiss && !errors.Is(err, sandbox.ErrPoolAgentUnsupported) {
				t.Fatalf("a router 404 = %v, want ErrPoolAgentUnsupported", err)
			}
			if !tc.wantOldMiss {
				if !errors.Is(err, sandbox.ErrNotFound) || errors.Is(err, sandbox.ErrPoolAgentUnsupported) {
					t.Fatalf("the agent's own 404 = %v, want ErrNotFound", err)
				}
			}
		})
	}
}
