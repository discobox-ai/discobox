package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/go-chi/chi/v5"
	"github.com/obot-platform/discobox/gormdb"
	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/server/internal/api"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/service"
)

func newStubRouterForTest() *chi.Mux {
	stubs := service.NewStub()
	router, _ := NewRouter(api.Services{
		Projects:     stubs,
		AgentConfigs: stubs,
		Sandboxes:    stubs,
		Providers:    stubs,
		Workers:      stubs,
		Events:       stubs,
	})
	return router
}

func newApplicationRouterTestDB(t *testing.T, ctx context.Context) *database.DB {
	t.Helper()
	db, err := database.New(database.Config{
		Driver: gormdb.DriverSQLite,
		DSN:    "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db"),
	})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close database: %v", err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	return db
}

func TestNewGeneratedRouterServesOpenAPIAndScalarDocs(t *testing.T) {
	stubs := service.NewStub()
	router, err := NewGeneratedRouter(api.Services{
		Projects:     stubs,
		AgentConfigs: stubs,
		Sandboxes:    stubs,
		Providers:    stubs,
		Workers:      stubs,
		Events:       stubs,
	})
	if err != nil {
		t.Fatalf("new generated router: %v", err)
	}
	assertOpenAPIAndScalarDocs(t, router)
}

func TestNewRouterServesOpenAPIAndScalarDocs(t *testing.T) {
	router := newStubRouterForTest()
	assertOpenAPIAndScalarDocs(t, router)
}

func assertOpenAPIAndScalarDocs(t *testing.T, router http.Handler) {
	t.Helper()

	openapiResp := httptest.NewRecorder()
	router.ServeHTTP(openapiResp, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if openapiResp.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json status = %d, want %d", openapiResp.Code, http.StatusOK)
	}
	if contentType := openapiResp.Header().Get("Content-Type"); !strings.Contains(contentType, "application/openapi+json") {
		t.Fatalf("GET /openapi.json content type = %q, want OpenAPI JSON", contentType)
	}
	if body := openapiResp.Body.String(); !strings.Contains(body, `"openapi"`) {
		t.Fatalf("GET /openapi.json body does not look like an OpenAPI document")
	}

	docsResp := httptest.NewRecorder()
	router.ServeHTTP(docsResp, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if docsResp.Code != http.StatusOK {
		t.Fatalf("GET /docs status = %d, want %d", docsResp.Code, http.StatusOK)
	}
	if body := docsResp.Body.String(); !strings.Contains(body, "@scalar/api-reference") {
		t.Fatalf("GET /docs body does not look like Scalar")
	}
	if body := docsResp.Body.String(); !strings.Contains(body, "/openapi.json") {
		t.Fatalf("GET /docs body does not reference /openapi.json")
	}
}

func jsonRequest(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestNewRouterCreateSandboxResolvesAgentName(t *testing.T) {
	router := newStubRouterForTest()

	createAgentResp := httptest.NewRecorder()
	router.ServeHTTP(createAgentResp, jsonRequest(http.MethodPost, "/projects/"+service.DefaultProjectID+"/agent-configs", `{
		"name": "Codex",
		"runCommand": "codex exec"
	}`))
	if createAgentResp.Code != http.StatusOK {
		t.Fatalf("POST /agent-configs status = %d, body = %s", createAgentResp.Code, createAgentResp.Body.String())
	}
	var agent model.AgentConfig
	if err := json.Unmarshal(createAgentResp.Body.Bytes(), &agent); err != nil {
		t.Fatalf("decode agent config: %v", err)
	}

	createSandboxResp := httptest.NewRecorder()
	router.ServeHTTP(createSandboxResp, jsonRequest(http.MethodPost, "/projects/"+service.DefaultProjectID+"/sandboxes", `{
		"name": "sandbox",
		"agentName": "Codex"
	}`))
	if createSandboxResp.Code != http.StatusAccepted {
		t.Fatalf("POST /sandboxes status = %d, body = %s", createSandboxResp.Code, createSandboxResp.Body.String())
	}
	var sandbox model.Sandbox
	if err := json.Unmarshal(createSandboxResp.Body.Bytes(), &sandbox); err != nil {
		t.Fatalf("decode sandbox: %v", err)
	}
	if sandbox.AgentConfigID == nil || *sandbox.AgentConfigID != agent.ID {
		t.Fatalf("agentConfigId = %v, want %q", sandbox.AgentConfigID, agent.ID)
	}
}

func TestNewApplicationRouterStartsWithDefaults(t *testing.T) {
	ctx := context.Background()
	db := newApplicationRouterTestDB(t, ctx)

	router, err := NewApplicationRouter(ctx, db.Write, db.Read, ApplicationRouterOptions{
		DispatcherEnabled: false,
	})
	if err != nil {
		t.Fatalf("new database router: %v", err)
	}

	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/projects", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /projects status = %d, body = %s", resp.Code, resp.Body.String())
	}

	var body struct {
		Projects []model.Project `json:"projects"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode projects: %v", err)
	}
	if len(body.Projects) != 1 {
		t.Fatalf("projects len = %d, want 1", len(body.Projects))
	}
	if body.Projects[0].ID != service.DefaultProjectID {
		t.Fatalf("project ID = %q, want %q", body.Projects[0].ID, service.DefaultProjectID)
	}
}

func TestNewApplicationRouterResolvesDefaultProjectAlias(t *testing.T) {
	ctx := context.Background()
	db := newApplicationRouterTestDB(t, ctx)

	router, err := NewApplicationRouter(ctx, db.Write, db.Read, ApplicationRouterOptions{
		DispatcherEnabled: false,
	})
	if err != nil {
		t.Fatalf("new database router: %v", err)
	}

	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/projects/default", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /projects/default status = %d, body = %s", resp.Code, resp.Body.String())
	}

	var project model.Project
	if err := json.Unmarshal(resp.Body.Bytes(), &project); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	if project.ID != service.DefaultProjectID {
		t.Fatalf("project ID = %q, want %q", project.ID, service.DefaultProjectID)
	}
	if !project.Default {
		t.Fatal("expected default project flag")
	}
}

func TestProjectStreamReceivesSandboxMutation(t *testing.T) {
	ctx := context.Background()
	db := newApplicationRouterTestDB(t, ctx)

	router, err := NewApplicationRouter(ctx, db.Write, db.Read, ApplicationRouterOptions{
		DispatcherEnabled: false,
	})
	if err != nil {
		t.Fatalf("new database router: %v", err)
	}
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	wsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, wsResp, err := websocket.Dial(wsCtx, "ws"+strings.TrimPrefix(server.URL, "http")+"/projects/default/stream", nil)
	if wsResp != nil && wsResp.Body != nil {
		defer wsResp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial project stream: %v", err)
	}
	defer func() {
		if err := conn.CloseNow(); err != nil {
			t.Fatalf("close project stream: %v", err)
		}
	}()

	list := false
	if err := wsjson.Write(wsCtx, conn, map[string]any{
		"type":   "subscribe",
		"stream": "sandbox",
		"list":   list,
	}); err != nil {
		t.Fatalf("subscribe project stream: %v", err)
	}

	readProjectStreamMessage(t, wsCtx, conn, "subscribed", "")
	readProjectStreamMessage(t, wsCtx, conn, "event", "connected")

	resp, err := http.Post(server.URL+"/projects/default/sandboxes", "application/json", strings.NewReader(`{"name":"live","description":"test sandbox"}`))
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create sandbox status = %d", resp.StatusCode)
	}

	msg := readProjectStreamMessage(t, wsCtx, conn, "event", model.EventTypeResourceChanged)
	var event model.ProjectEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		t.Fatalf("decode project event: %v", err)
	}
	if event.ResourceType != "sandbox" || event.Action != model.EventActionCreated {
		t.Fatalf("event = %#v, want sandbox created event", event)
	}
}

type projectStreamTestMessage struct {
	Type  string          `json:"type"`
	Event string          `json:"event,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

func readProjectStreamMessage(t *testing.T, ctx context.Context, conn *websocket.Conn, wantType, wantEvent string) projectStreamTestMessage {
	t.Helper()
	for {
		var msg projectStreamTestMessage
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			t.Fatalf("read project stream: %v", err)
		}
		if msg.Type == wantType && (wantEvent == "" || msg.Event == wantEvent) {
			return msg
		}
	}
}
