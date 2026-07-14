package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"
	"github.com/obot-platform/discobox/sandbox-agent/config"
	"github.com/obot-platform/discobox/sandbox-agent/execs"
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
	if body := resp.Body.String(); !strings.Contains(body, `"status":"starting"`) || !strings.Contains(body, `"command":["codex"]`) {
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
		ImageConfig: config.ImageConfig{Env: map[string]string{"PATH": "/usr/bin"}},
	})
	if err != nil {
		t.Fatalf("new exec manager: %v", err)
	}
	created, err := terminal.NewService(terminal.ServiceConfig{
		Execs:                 execManager,
		ResolvedHarnessConfig: cfg.ResolvedHarnessConfig,
		Harnesses:             cfg.Harnesses,
		WorkingRoot:           cfg.WorkingRoot,
		RuntimeDir:            cfg.RuntimeDir,
		ImageConfig:           config.ImageConfig{Env: map[string]string{"PATH": "/usr/bin"}},
		Units:                 runner,
		Installer:             cfg.Installer,
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
			WorkerID:  "worker-1",
		},
		ControlPlanePublicKey: publicKey,
		ListenAddress:         ":0",
		WorkingRoot:           "/workspace",
		RuntimeDir:            runtimeDir,
		DatabasePath:          filepath.Join(runtimeDir, "harness.db"),
		Harnesses: []config.Harness{{
			ID:      "codex",
			Command: []string{"codex"},
		}},
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
			token.SetString("worker_id", workerID)
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
