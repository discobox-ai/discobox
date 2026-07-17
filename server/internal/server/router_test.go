package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/go-chi/chi/v5"
	serverapi "github.com/obot-platform/discobox/api/gen"
	"github.com/obot-platform/discobox/gormdb"
	"github.com/obot-platform/discobox/server/internal/auth"
	workeragentauth "github.com/obot-platform/discobox/server/internal/auth/workeragent"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/service"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/transport"
)

func newStubRouterForTest() *chi.Mux {
	stubs := newRouterTestServices()
	router, _ := NewRouter(services.Services{
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Workers:        stubs,
		Jobs:           stubs,
		Events:         stubs,
	})
	return router
}

func newAppTestDB(ctx context.Context, t *testing.T) *database.DB {
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

func TestNewOpenAPIRouterServesOpenAPIAndScalarDocs(t *testing.T) {
	stubs := newRouterTestServices()
	router, err := NewOpenAPIRouter(services.Services{
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Workers:        stubs,
		Jobs:           stubs,
		Events:         stubs,
	})
	if err != nil {
		t.Fatalf("new OpenAPI router: %v", err)
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
	router.ServeHTTP(openapiResp, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/openapi.yaml", nil))
	if openapiResp.Code != http.StatusOK {
		t.Fatalf("GET /openapi.yaml status = %d, want %d", openapiResp.Code, http.StatusOK)
	}
	if contentType := openapiResp.Header().Get("Content-Type"); !strings.Contains(contentType, "application/openapi+yaml") {
		t.Fatalf("GET /openapi.yaml content type = %q, want OpenAPI YAML", contentType)
	}
	if body := openapiResp.Body.String(); !strings.Contains(body, "openapi:") {
		t.Fatalf("GET /openapi.yaml body does not look like an OpenAPI document")
	}

	docsResp := httptest.NewRecorder()
	router.ServeHTTP(docsResp, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/docs", nil))
	if docsResp.Code != http.StatusOK {
		t.Fatalf("GET /docs status = %d, want %d", docsResp.Code, http.StatusOK)
	}
	if body := docsResp.Body.String(); !strings.Contains(body, "@scalar/api-reference") {
		t.Fatalf("GET /docs body does not look like Scalar")
	}
	if body := docsResp.Body.String(); !strings.Contains(body, "/openapi.yaml") {
		t.Fatalf("GET /docs body does not reference /openapi.yaml")
	}
}

func jsonRequest(method, target, body string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func scopedUserRequest(ctx context.Context, method, target string, body io.Reader, scopes ...string) *http.Request {
	req := httptest.NewRequestWithContext(ctx, method, target, body)
	principal := auth.Principal{
		Type:   auth.PrincipalTypeUser,
		UserID: service.DefaultUserID,
		Scopes: scopes,
	}
	return req.WithContext(auth.WithPrincipal(req.Context(), principal))
}

func TestNewRouterCreateSandboxResolvesHarnessName(t *testing.T) {
	router := newStubRouterForTest()

	createHarnessResp := httptest.NewRecorder()
	router.ServeHTTP(createHarnessResp, jsonRequest(http.MethodPost, "/projects/"+service.DefaultProjectID+"/harness-configs", `{
		"name": "Codex",
		"image": "discobox-harness-codex:local"
	}`))
	if createHarnessResp.Code != http.StatusOK {
		t.Fatalf("POST /harness-configs status = %d, body = %s", createHarnessResp.Code, createHarnessResp.Body.String())
	}
	var harness model.HarnessConfig
	if err := json.Unmarshal(createHarnessResp.Body.Bytes(), &harness); err != nil {
		t.Fatalf("decode harness config: %v", err)
	}

	createSandboxResp := httptest.NewRecorder()
	router.ServeHTTP(createSandboxResp, jsonRequest(http.MethodPost, "/projects/"+service.DefaultProjectID+"/sandboxes", `{
		"harnessName": "Codex",
		"config": {
			"name": "sandbox",
			"user": {
				"name": "darren",
				"uid": 1002,
				"gid": 1002,
				"homeDirectory": "/home/darren"
			}
		}
	}`))
	if createSandboxResp.Code != http.StatusAccepted {
		t.Fatalf("POST /sandboxes status = %d, body = %s", createSandboxResp.Code, createSandboxResp.Body.String())
	}
	var sandbox serverapi.Sandbox
	if err := json.Unmarshal(createSandboxResp.Body.Bytes(), &sandbox); err != nil {
		t.Fatalf("decode sandbox: %v", err)
	}
	if got := sandbox.Config.HarnessConfigId.Or(""); got != harness.ID {
		t.Fatalf("harnessConfigId = %q, want %q", got, harness.ID)
	}
}

func TestNewRouterGeneratedErrorsUseProblemJSON(t *testing.T) {
	router := newStubRouterForTest()

	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/projects/"+service.DefaultProjectID+"/sandboxes", strings.NewReader(`{"name":`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("POST /sandboxes status = %d, want %d", resp.Code, http.StatusBadRequest)
	}
	if got := resp.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", got)
	}

	var body struct {
		Status int    `json:"status"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode problem body: %v", err)
	}
	if body.Status != http.StatusBadRequest || body.Title != http.StatusText(http.StatusBadRequest) || body.Detail == "" {
		t.Fatalf("problem body = %#v", body)
	}
}

func TestSandboxGitRepositoryRouteProxiesToWorker(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	workerID := "worker-1"
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       service.DefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		WorkerID:        &workerID,
	}
	var released bool
	projectID := service.DefaultProjectID
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/project/" + projectID + "/worker/worker-1/sandboxes/sandbox-1/git-repositories/primary.git/info/refs"
		if r.URL.Path != wantPath {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		if r.URL.RawQuery != "service=git-upload-pack" {
			t.Fatalf("upstream query = %q", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer worker-token" {
			t.Fatalf("upstream auth = %q", got)
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = w.Write([]byte("git response"))
	}))
	t.Cleanup(upstream.Close)
	stubs.sandboxLease = transport.NewHTTPClientLeaseWithBaseURLAndAuth(upstream.Client(), upstream.URL, "worker-token", func() {
		released = true
	})

	router, err := NewRouter(services.Services{
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Workers:        stubs,
		Jobs:           stubs,
		Events:         stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodGet, "/projects/"+projectID+"/sandboxes/sandbox-1/git-repositories/primary.git/info/refs?service=git-upload-pack", nil, workeragentauth.ScopeSandboxRead)
	req.Header.Set("Authorization", "Bearer user-token")
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("GET git info/refs status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if body := resp.Body.String(); body != "git response" {
		t.Fatalf("body = %q, want git response", body)
	}
	if !released {
		t.Fatal("expected sandbox HTTP lease to be released")
	}
	if !slices.Equal(stubs.sandboxScopes, []string{workeragentauth.ScopeSandboxRead}) {
		t.Fatalf("sandbox HTTP scopes = %#v", stubs.sandboxScopes)
	}
}

func TestSandboxGitRepositoryRouteUsesWriteScopeForReceivePack(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	workerID := "worker-1"
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       service.DefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		WorkerID:        &workerID,
	}
	projectID := service.DefaultProjectID
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "service=git-receive-pack" {
			t.Fatalf("upstream query = %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		_, _ = w.Write([]byte("git receive response"))
	}))
	t.Cleanup(upstream.Close)
	stubs.sandboxLease = transport.NewHTTPClientLeaseWithBaseURLAndAuth(upstream.Client(), upstream.URL, "worker-token", nil)

	router, err := NewRouter(services.Services{
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Workers:        stubs,
		Jobs:           stubs,
		Events:         stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodGet, "/projects/"+projectID+"/sandboxes/sandbox-1/git-repositories/primary.git/info/refs?service=git-receive-pack", nil, workeragentauth.ScopeSandboxWrite)
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("GET git receive info/refs status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if !slices.Equal(stubs.sandboxScopes, []string{workeragentauth.ScopeSandboxWrite}) {
		t.Fatalf("sandbox HTTP scopes = %#v", stubs.sandboxScopes)
	}
}

func TestSandboxHTTPRouteProxiesPortToWorker(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	workerID := "worker-1"
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       service.DefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		WorkerID:        &workerID,
	}
	var released bool
	projectID := service.DefaultProjectID
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/project/" + projectID + "/worker/worker-1/sandboxes/sandbox-1/http/8080/api/status"
		if r.URL.Path != wantPath {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		if r.URL.RawQuery != "verbose=true" {
			t.Fatalf("upstream query = %q", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer worker-token" {
			t.Fatalf("upstream auth = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)
	stubs.sandboxLease = transport.NewHTTPClientLeaseWithBaseURLAndAuth(upstream.Client(), upstream.URL, "worker-token", func() {
		released = true
	})

	router, err := NewRouter(services.Services{
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Workers:        stubs,
		Jobs:           stubs,
		Events:         stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodGet, "/projects/"+projectID+"/sandboxes/sandbox-1/http/8080/api/status?verbose=true", nil, workeragentauth.ScopeSandboxHTTP)
	req.Header.Set("Authorization", "Bearer user-token")
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("GET sandbox HTTP status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if body := resp.Body.String(); body != `{"ok":true}` {
		t.Fatalf("body = %q, want proxied response", body)
	}
	if !released {
		t.Fatal("expected sandbox HTTP lease to be released")
	}
	if !slices.Equal(stubs.sandboxScopes, []string{workeragentauth.ScopeSandboxHTTP}) {
		t.Fatalf("sandbox HTTP scopes = %#v", stubs.sandboxScopes)
	}
}

func TestSandboxExecListRouteProxiesToSandboxAgent(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	workerID := "worker-1"
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       service.DefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		WorkerID:        &workerID,
	}
	var released bool
	projectID := service.DefaultProjectID
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/project/" + projectID + "/worker/worker-1/sandboxes/sandbox-1/execs"
		if r.URL.Path != wantPath {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer worker-token" {
			t.Fatalf("upstream auth = %q", got)
		}
		if got := r.Header.Get("X-Discobox-Sandbox-Agent-Authorization"); got != "Bearer sandbox-agent-token" {
			t.Fatalf("upstream sandbox-agent auth = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"execs":[]}`))
	}))
	t.Cleanup(upstream.Close)
	stubs.sandboxLease = transport.NewHTTPClientLeaseWithBaseURLAndAuth(upstream.Client(), upstream.URL, "worker-token", func() {
		released = true
	})
	stubs.sandboxLease.ForwardAuthTokenProvider = func(context.Context) (string, error) {
		return "sandbox-agent-token", nil
	}

	router, err := NewRouter(services.Services{
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Workers:        stubs,
		Jobs:           stubs,
		Events:         stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodGet, "/api/projects/"+projectID+"/sandboxes/sandbox-1/execs", nil, workeragentauth.ScopeExecRead)
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("GET sandbox agent terminals status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if body := resp.Body.String(); body != `{"execs":[]}` {
		t.Fatalf("body = %q, want proxied response", body)
	}
	if !released {
		t.Fatal("expected sandbox HTTP lease to be released")
	}
	if !slices.Equal(stubs.sandboxScopes, []string{workeragentauth.ScopeExecRead}) {
		t.Fatalf("sandbox HTTP scopes = %#v", stubs.sandboxScopes)
	}
}

func TestSandboxExecProxyErrorUsesJSON(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	workerID := "worker-1"
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       service.DefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		WorkerID:        &workerID,
	}
	projectID := service.DefaultProjectID

	router, err := NewRouter(services.Services{
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Workers:        stubs,
		Jobs:           stubs,
		Events:         stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodPost, "/api/projects/"+projectID+"/sandboxes/sandbox-1/execs", nil, workeragentauth.ScopeExecWrite)
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST sandbox agent terminal status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want application/json; charset=utf-8", got)
	}
	var body serverapi.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body.Error != "sandbox HTTP client is not available" {
		t.Fatalf("error = %q", body.Error)
	}
}

func TestSandboxExecAttachRouteUsesWriteScope(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	workerID := "worker-1"
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       service.DefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		WorkerID:        &workerID,
	}
	projectID := service.DefaultProjectID
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/project/" + projectID + "/worker/worker-1/sandboxes/sandbox-1/execs/exec-1/attach"
		if r.URL.Path != wantPath {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer worker-token" {
			t.Fatalf("upstream auth = %q", got)
		}
		if got := r.Header.Get("X-Discobox-Sandbox-Agent-Authorization"); got != "Bearer sandbox-agent-token" {
			t.Fatalf("upstream sandbox-agent auth = %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	stubs.sandboxLease = transport.NewHTTPClientLeaseWithBaseURLAndAuth(upstream.Client(), upstream.URL, "worker-token", nil)
	stubs.sandboxLease.ForwardAuthTokenProvider = func(context.Context) (string, error) {
		return "sandbox-agent-token", nil
	}

	router, err := NewRouter(services.Services{
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Workers:        stubs,
		Jobs:           stubs,
		Events:         stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodPost, "/api/projects/"+projectID+"/sandboxes/sandbox-1/execs/exec-1/attach", nil, workeragentauth.ScopeExecWrite)
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusNoContent {
		t.Fatalf("POST sandbox agent terminal attach status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if !slices.Equal(stubs.sandboxScopes, []string{workeragentauth.ScopeExecWrite, workeragentauth.ScopeExecRead}) {
		t.Fatalf("sandbox HTTP scopes = %#v", stubs.sandboxScopes)
	}
}

func TestSandboxHarnessHookRouteUsesExecReadScope(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	workerID := "worker-1"
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       service.DefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		WorkerID:        &workerID,
	}
	projectID := service.DefaultProjectID
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/project/" + projectID + "/worker/worker-1/sandboxes/sandbox-1/harness-hooks"
		if r.URL.Path != wantPath {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"hooks":[]}`))
	}))
	t.Cleanup(upstream.Close)
	stubs.sandboxLease = transport.NewHTTPClientLeaseWithBaseURLAndAuth(upstream.Client(), upstream.URL, "worker-token", nil)
	stubs.sandboxLease.ForwardAuthTokenProvider = func(context.Context) (string, error) {
		return "sandbox-agent-token", nil
	}

	router, err := NewRouter(services.Services{
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Workers:        stubs,
		Jobs:           stubs,
		Events:         stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodGet, "/api/projects/"+projectID+"/sandboxes/sandbox-1/harness-hooks", nil, workeragentauth.ScopeExecRead)
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("GET harness hooks status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if !slices.Equal(stubs.sandboxScopes, []string{workeragentauth.ScopeExecRead}) {
		t.Fatalf("sandbox HTTP scopes = %#v", stubs.sandboxScopes)
	}
}

func TestSandboxExecRoutesUseExecScopes(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	workerID := "worker-1"
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       service.DefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		WorkerID:        &workerID,
	}
	projectID := service.DefaultProjectID
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/project/" + projectID + "/worker/worker-1/sandboxes/sandbox-1/execs"
		if r.URL.Path != wantPath {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"execs":[]}`))
	}))
	t.Cleanup(upstream.Close)
	stubs.sandboxLease = transport.NewHTTPClientLeaseWithBaseURLAndAuth(upstream.Client(), upstream.URL, "worker-token", nil)
	stubs.sandboxLease.ForwardAuthTokenProvider = func(context.Context) (string, error) {
		return "sandbox-agent-token", nil
	}

	router, err := NewRouter(services.Services{
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Workers:        stubs,
		Jobs:           stubs,
		Events:         stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodGet, "/api/projects/"+projectID+"/sandboxes/sandbox-1/execs", nil, workeragentauth.ScopeExecRead)
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("GET sandbox execs status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if !slices.Equal(stubs.sandboxScopes, []string{workeragentauth.ScopeExecRead}) {
		t.Fatalf("sandbox HTTP scopes = %#v", stubs.sandboxScopes)
	}
}

func TestSandboxExecAttachRouteUsesExecWriteScope(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	workerID := "worker-1"
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       service.DefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		WorkerID:        &workerID,
	}
	projectID := service.DefaultProjectID
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/project/" + projectID + "/worker/worker-1/sandboxes/sandbox-1/execs/exec-1/attach"
		if r.URL.Path != wantPath {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		http.Error(w, "Forbidden", http.StatusForbidden)
	}))
	t.Cleanup(upstream.Close)
	stubs.sandboxLease = transport.NewHTTPClientLeaseWithBaseURLAndAuth(upstream.Client(), upstream.URL, "worker-token", nil)
	stubs.sandboxLease.ForwardAuthTokenProvider = func(context.Context) (string, error) {
		return "sandbox-agent-token", nil
	}

	router, err := NewRouter(services.Services{
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Workers:        stubs,
		Jobs:           stubs,
		Events:         stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodGet, "/api/projects/"+projectID+"/sandboxes/sandbox-1/execs/exec-1/attach", nil, workeragentauth.ScopeExecWrite)
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("GET sandbox exec attach status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if !slices.Equal(stubs.sandboxScopes, []string{workeragentauth.ScopeExecWrite, workeragentauth.ScopeExecRead}) {
		t.Fatalf("sandbox HTTP scopes = %#v", stubs.sandboxScopes)
	}
}

func TestNewAppStartsWithDefaults(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)

	router, err := NewApp(ctx, db.Write, db.Read)
	if err != nil {
		t.Fatalf("new database router: %v", err)
	}

	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/projects", nil))
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

func TestNewAppResolvesDefaultProjectAlias(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)

	router, err := NewApp(ctx, db.Write, db.Read)
	if err != nil {
		t.Fatalf("new database router: %v", err)
	}

	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/projects/default", nil))
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
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)

	router, err := NewApp(ctx, db.Write, db.Read)
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

	readProjectStreamMessage(wsCtx, t, conn, "subscribed", "")
	readProjectStreamMessage(wsCtx, t, conn, "event", "connected")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/projects/default/sandboxes", strings.NewReader(`{"config":{"name":"live","description":"test sandbox"}}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create sandbox status = %d: %s", resp.StatusCode, string(body))
	}

	msg := readProjectStreamMessage(wsCtx, t, conn, "event", model.EventTypeResourceChanged)
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

func readProjectStreamMessage(ctx context.Context, t *testing.T, conn *websocket.Conn, wantType, wantEvent string) projectStreamTestMessage {
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
