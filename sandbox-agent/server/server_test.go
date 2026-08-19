package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"
	sandboxapi "github.com/obot-platform/discobox/api/sandboxgen"
	"github.com/obot-platform/discobox/sandbox-agent/config"
	"github.com/obot-platform/discobox/sandbox-agent/execs"
	"github.com/obot-platform/discobox/sandbox-agent/ports"
	agentstore "github.com/obot-platform/discobox/sandbox-agent/store"
	"github.com/obot-platform/discobox/sandbox-agent/terminal"
)

func TestHealthDoesNotRequireAuth(t *testing.T) {
	publicKey, _ := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil))

	if resp.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, body = %s", resp.Code, resp.Body.String())
	}
}

func TestListHarnessHooksRequiresExecReadScope(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/harness-hooks", nil)
	req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeExecRead))
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("GET harness hooks status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if body := resp.Body.String(); body != `{"hooks":[]}` {
		t.Fatalf("body = %q", body)
	}
}

func TestListHarnessHooksRejectsWriteOnlyScope(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/harness-hooks", nil)
	req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeExecWrite))
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("GET harness hooks status = %d, body = %s", resp.Code, resp.Body.String())
	}
}

func TestGetSandboxAgentStatusRequiresStatusReadScope(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/status", nil)
	req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeStatusRead))
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("GET status status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var body struct {
		Sources    []any  `json:"sources"`
		Sessions   []any  `json:"sessions"`
		Ports      []any  `json:"ports"`
		ObservedAt string `json:"observedAt"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v, body = %s", err, resp.Body.String())
	}
	if body.Sources == nil || len(body.Sources) != 0 {
		t.Fatalf("sources = %v, want an empty array (no sources configured)", body.Sources)
	}
	if body.Ports == nil || len(body.Ports) != 0 {
		t.Fatalf("ports = %v, want an empty array (nothing listening)", body.Ports)
	}
	if body.ObservedAt == "" {
		t.Fatal("observedAt is empty")
	}
}

// TestGetSandboxAgentStatusReportsWatchedPorts covers the one status component
// read from a snapshot rather than computed per request (ADR 0046).
func TestGetSandboxAgentStatusReportsWatchedPorts(t *testing.T) {
	procRoot := filepath.Join(t.TempDir(), "proc")
	if err := os.MkdirAll(filepath.Join(procRoot, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	table := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 0100007F:1435 00000000:0000 0A 00000000:00000000 00:00000000 00000000  4242        0 51001 1 0000 100 0 0 10 0\n"
	if err := os.WriteFile(filepath.Join(procRoot, "net", "tcp"), []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	watcher := ports.New(ports.Config{
		UID:      4242,
		ProcRoot: procRoot,
		Probe:    func(context.Context, netip.AddrPort) ports.Protocol { return ports.ProtocolHTTP },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watcher.Run(ctx)

	agent := &handler{ports: watcher}
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, err := agent.GetSandboxAgentStatus(ctx, sandboxapi.GetSandboxAgentStatusParams{})
		if err != nil {
			t.Fatalf("get status: %v", err)
		}
		if len(status.Ports) == 1 && status.Ports[0].Protocol == "http" {
			if status.Ports[0].Port != 5173 {
				t.Fatalf("port = %d, want 5173", status.Ports[0].Port)
			}
			if got := status.Ports[0].Addresses; len(got) != 1 || got[0] != "127.0.0.1" {
				t.Fatalf("addresses = %v, want [127.0.0.1]", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status never reported the watched port: %+v", status.Ports)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestGetSandboxAgentStatusRejectsBroaderScopeAlone confirms the status route
// is gated on its own scope rather than piggybacking on exec:read — a token
// scoped only for execs must not be able to read status.
func TestGetSandboxAgentStatusRejectsBroaderScopeAlone(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/status", nil)
	req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeExecRead, ScopeTerminalRead))
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("GET status status = %d, want forbidden; body = %s", resp.Code, resp.Body.String())
	}
}

// TestGetSandboxAgentStatusStatusReadScopeCannotReadExecs confirms the scope
// ceiling holds the other direction too: a status:read-only token must not
// unlock exec routes.
func TestGetSandboxAgentStatusStatusReadScopeCannotReadExecs(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/execs", nil)
	req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeStatusRead))
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("GET execs status = %d, want forbidden; body = %s", resp.Code, resp.Body.String())
	}
}

func TestListSandboxExecsRequiresExecReadScope(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/execs", nil)
	req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeExecRead))
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("GET sandbox execs status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if body := resp.Body.String(); body != `{"execs":[]}` {
		t.Fatalf("body = %q", body)
	}
}

func TestListSandboxExecsRejectsTerminalReadScope(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/execs", nil)
	req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeTerminalRead))
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("GET sandbox execs status = %d, body = %s", resp.Code, resp.Body.String())
	}
}

func TestAttachSandboxExecRequiresExecWriteScope(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	readResp := httptest.NewRecorder()
	readReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/execs/exec-1/attach", nil)
	readReq.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeExecRead))
	router.ServeHTTP(readResp, readReq)
	if readResp.Code != http.StatusForbidden {
		t.Fatalf("GET sandbox exec attach with read scope status = %d, body = %s", readResp.Code, readResp.Body.String())
	}

	writeResp := httptest.NewRecorder()
	writeReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/execs/exec-1/attach", nil)
	writeReq.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeExecWrite))
	router.ServeHTTP(writeResp, writeReq)
	if writeResp.Code == http.StatusForbidden {
		t.Fatalf("GET sandbox exec attach with write scope status = %d, body = %s", writeResp.Code, writeResp.Body.String())
	}
}

func TestCreateAgentTerminalRequiresWriteScope(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	runner := &sandboxAgentFakeRunner{}
	router, err := NewRouter(testConfigWithRunner(publicKey, runner))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/projects/project-1/sandboxes/sandbox-1/execs", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeExecWrite))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusCreated {
		t.Fatalf("POST harness terminals status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if len(runner.starts) != 1 {
		t.Fatalf("starts = %#v", runner.starts)
	}
	// The exec's command is the login shell it actually runs as (real job
	// control); the harness command is what got typed into it, reported
	// separately as startupCommand.
	if body := resp.Body.String(); !strings.Contains(body, `"status":"starting"`) || !strings.Contains(body, `"startupCommand":["codex"]`) {
		t.Fatalf("body = %q", body)
	}
}

func TestTokenRouteIdentityMustMatch(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/other-sandbox/execs", nil)
	req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeExecRead))
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("GET harness terminals status = %d, body = %s", resp.Code, resp.Body.String())
	}
}

func TestAgentTerminalEventsAndResources(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	st, err := agentstore.Open(context.Background(), filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	runner := &sandboxAgentFakeRunner{}
	cfg := testConfigWithRunner(publicKey, runner)
	cfg.Store = st
	cfg.ExecAuditRecorder = nil
	cfg.Resources.RetentionCount = 2
	router, err := NewRouter(cfg)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	createResp := httptest.NewRecorder()
	createReq := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/projects/project-1/sandboxes/sandbox-1/execs", strings.NewReader(`{}`))
	createReq.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeExecWrite))
	createReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("POST terminal status = %d, body = %s", createResp.Code, createResp.Body.String())
	}
	var created struct {
		Exec struct {
			ID string `json:"id"`
		} `json:"exec"`
	}
	if err := json.Unmarshal(createResp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	eventsResp := httptest.NewRecorder()
	eventsReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/execs/"+created.Exec.ID+"/events?limit=10", nil)
	eventsReq.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeExecRead))
	router.ServeHTTP(eventsResp, eventsReq)
	if eventsResp.Code != http.StatusOK {
		t.Fatalf("GET events status = %d, body = %s", eventsResp.Code, eventsResp.Body.String())
	}
	if body := eventsResp.Body.String(); !strings.Contains(body, `"type":"exec.created"`) || !strings.Contains(body, `"events"`) {
		t.Fatalf("events body = %s", body)
	}

	resourceResp := httptest.NewRecorder()
	resourceReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/execs/"+created.Exec.ID+"/resources", nil)
	resourceReq.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeExecRead))
	router.ServeHTTP(resourceResp, resourceReq)
	if resourceResp.Code != http.StatusOK {
		t.Fatalf("GET resources status = %d, body = %s", resourceResp.Code, resourceResp.Body.String())
	}
	if body := resourceResp.Body.String(); !strings.Contains(body, `"terminalId":"`+created.Exec.ID+`"`) || !strings.Contains(body, `"data":`) {
		t.Fatalf("resource body = %s", body)
	}

	historyResp := httptest.NewRecorder()
	historyReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/execs/"+created.Exec.ID+"/resources/history?limit=1", nil)
	historyReq.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeExecRead))
	router.ServeHTTP(historyResp, historyReq)
	if historyResp.Code != http.StatusOK {
		t.Fatalf("GET resource history status = %d, body = %s", historyResp.Code, historyResp.Body.String())
	}
	if body := historyResp.Body.String(); !strings.Contains(body, `"snapshots":[`) || !strings.Contains(body, `"terminalId":"`+created.Exec.ID+`"`) {
		t.Fatalf("history body = %s", body)
	}
}

func TestAgentTerminalResourceStreamReplaysHistory(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	st, err := agentstore.Open(context.Background(), filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	runner := &sandboxAgentFakeRunner{}
	cfg := testConfigWithRunner(publicKey, runner)
	cfg.Store = st
	cfg.ExecAuditRecorder = nil
	cfg.Resources.SampleInterval = time.Hour
	router, err := NewRouter(cfg)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	execManager, err := execs.NewManagerWithConfig(execs.ManagerConfig{
		WorkingRoot: cfg.WorkingRoot,
		RuntimeDir:  cfg.RuntimeDir,
		Units:       runner,
		Audit:       st,
		Env:         map[string]string{"PATH": "/usr/bin"},
	})
	if err != nil {
		t.Fatalf("new exec manager: %v", err)
	}
	created, err := terminal.NewService(terminal.ServiceConfig{
		Execs:       execManager,
		Harness:     cfg.Harness,
		WorkingRoot: cfg.WorkingRoot,
		RuntimeDir:  cfg.RuntimeDir,
		Env:         map[string]string{"PATH": "/usr/bin"},
		Units:       runner,
		Installer:   cfg.Installer,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	term, err := created.Create(context.Background(), terminal.CreateRequest{})
	if err != nil {
		t.Fatalf("create terminal directly: %v", err)
	}
	if _, err := st.RecordResourceSample(context.Background(), agentstore.ResourceSample{
		TerminalID: term.ID,
		SampledAt:  time.Now().UTC(),
		Source:     "test",
		Data:       []byte(`{"from":"history"}`),
	}, 10); err != nil {
		t.Fatalf("record resource sample: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/execs/"+term.ID+"/resources/stream", nil)
	req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeExecRead))
	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET stream status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if body := resp.Body.String(); !strings.Contains(body, "event: resource") || !strings.Contains(body, `"from":"history"`) {
		t.Fatalf("stream body = %s", body)
	}
}

func testConfig(publicKey string) Config {
	runtimeDir, _ := os.MkdirTemp("", "discobox-sandbox-agent-test-*")
	return Config{
		Identity: Identity{
			ProjectID: "project-1",
			SandboxID: "sandbox-1",
			PoolID:    "worker-1",
		},
		ControlPlanePublicKey: publicKey,
		ListenAddress:         ":0",
		WorkingRoot:           "/workspace",
		RuntimeDir:            runtimeDir,
		DatabasePath:          filepath.Join(runtimeDir, "harness.db"),
		Harness: config.Harness{
			ID:      "codex",
			Command: []string{"codex"},
		},
		ExecUnitManager:   &sandboxAgentFakeRunner{},
		Installer:         sandboxAgentNoopInstaller{},
		ExecAuditRecorder: sandboxAgentNoopAudit{},
	}
}

func testConfigWithRunner(publicKey string, runner execs.UnitManager) Config {
	cfg := testConfig(publicKey)
	cfg.ExecUnitManager = runner
	return cfg
}

func sandboxAgentTestSigner(t *testing.T) (string, func(projectID, sandboxID, workerID string, scopes ...string) string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	secretKey, err := paseto.NewV4AsymmetricSecretKeyFromEd25519(privateKey)
	if err != nil {
		t.Fatalf("load secret key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(publicKey), func(projectID, sandboxID, workerID string, scopes ...string) string {
		now := time.Now()
		token := paseto.NewToken()
		token.SetAudience(SandboxAgentAudience)
		token.SetIssuedAt(now)
		token.SetNotBefore(now.Add(-time.Minute))
		token.SetExpiration(now.Add(time.Hour))
		token.SetString("project_id", projectID)
		token.SetString("sandbox_id", sandboxID)
		if workerID != "" {
			token.SetString("pool_id", workerID)
		}
		if err := token.Set("scopes", scopes); err != nil {
			t.Fatalf("set scopes: %v", err)
		}
		return token.V4Sign(secretKey, nil)
	}
}

type sandboxAgentFakeRunner struct {
	starts []execs.StartRequest
	stops  []string
}

func (r *sandboxAgentFakeRunner) Start(_ context.Context, req execs.StartRequest) (execs.StartResult, error) {
	r.starts = append(r.starts, req)
	return execs.StartResult{Unit: req.Unit, PID: 1234}, nil
}

func (r *sandboxAgentFakeRunner) Stop(_ context.Context, unit string) error {
	r.stops = append(r.stops, unit)
	return nil
}

func (r *sandboxAgentFakeRunner) Status(context.Context, string) (execs.UnitStatus, error) {
	return execs.UnitStatus{}, os.ErrNotExist
}

func (r *sandboxAgentFakeRunner) List(context.Context) ([]execs.UnitStatus, error) {
	return nil, nil
}

type sandboxAgentNoopAudit struct{}

type sandboxAgentNoopInstaller struct{}

func (sandboxAgentNoopInstaller) EnsureInstalled(context.Context, config.Harness, string, map[string]string) error {
	return nil
}

func (sandboxAgentNoopInstaller) RestoreSecretFiles(context.Context, config.Harness, map[string]string) ([]string, error) {
	return nil, nil
}

func (sandboxAgentNoopAudit) RecordExecEvent(context.Context, string, string, string, map[string]any) error {
	return nil
}

func (sandboxAgentNoopAudit) ObserveExec(context.Context, execs.Exec) error {
	return nil
}

func (sandboxAgentNoopAudit) SaveExecRecord(context.Context, execs.Exec) error {
	return nil
}

func (sandboxAgentNoopAudit) LoadExecRecords(context.Context) ([]execs.Exec, error) {
	return nil, nil
}
