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

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/gormdb"
	"github.com/discobox-ai/discobox/id"
	"github.com/discobox-ai/discobox/server/internal/auth"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/service"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/transport"
	"github.com/go-chi/chi/v5"
)

func newStubRouterForTest() *chi.Mux {
	stubs := newRouterTestServices()
	router, _ := NewRouter(services.Services{
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Pools:          stubs,
		Jobs:           stubs,
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
		Pools:          stubs,
		Jobs:           stubs,
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
	router.ServeHTTP(createHarnessResp, jsonRequest(http.MethodPost, "/projects/"+testDefaultProjectID+"/harness-configs", `{
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
	router.ServeHTTP(createSandboxResp, jsonRequest(http.MethodPost, "/projects/"+testDefaultProjectID+"/sandboxes", `{
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
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/projects/"+testDefaultProjectID+"/sandboxes", strings.NewReader(`{"name":`))
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

// A push into a source's origin repository takes its own route to its own
// repository on the pool, and needs the write scope because it is a
// receive-pack (ADR 0058 §3).
func TestSandboxGitOriginRouteProxiesReceivePackToPool(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       testDefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		PoolID:          "pool-1",
	}
	projectID := testDefaultProjectID
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/project/" + projectID + "/pool/pool-1/sandboxes/sandbox-1/git-origins/primary.git/git-receive-pack"
		if r.URL.Path != wantPath {
			t.Fatalf("upstream path = %q, want %q", r.URL.Path, wantPath)
		}
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		_, _ = w.Write([]byte("push accepted"))
	}))
	t.Cleanup(upstream.Close)
	stubs.sandboxLease = transport.NewHTTPClientLeaseWithBaseURLAndAuth(upstream.Client(), upstream.URL, "worker-token", func() {})

	router, err := NewRouter(services.Services{
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Pools:          stubs,
		Jobs:           stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodPost, "/projects/"+projectID+"/sandboxes/sandbox-1/git-origins/primary.git/git-receive-pack", nil, poolagentauth.ScopeSandboxWrite)
	req.Header.Set("Authorization", "Bearer user-token")
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("POST git-receive-pack status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if body := resp.Body.String(); body != "push accepted" {
		t.Fatalf("body = %q, want push accepted", body)
	}
	if !slices.Equal(stubs.sandboxScopes, []string{poolagentauth.ScopeSandboxWrite}) {
		t.Fatalf("sandbox HTTP scopes = %#v", stubs.sandboxScopes)
	}
}

func TestSandboxGitRepositoryRouteProxiesToPool(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       testDefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		PoolID:          "pool-1",
	}
	var released bool
	projectID := testDefaultProjectID
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/project/" + projectID + "/pool/pool-1/sandboxes/sandbox-1/git-repositories/primary.git/info/refs"
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
		Pools:          stubs,
		Jobs:           stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodGet, "/projects/"+projectID+"/sandboxes/sandbox-1/git-repositories/primary.git/info/refs?service=git-upload-pack", nil, poolagentauth.ScopeSandboxRead)
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
	if !slices.Equal(stubs.sandboxScopes, []string{poolagentauth.ScopeSandboxRead}) {
		t.Fatalf("sandbox HTTP scopes = %#v", stubs.sandboxScopes)
	}
}

func TestSandboxGitRepositoryRouteUsesWriteScopeForReceivePack(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       testDefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		PoolID:          "pool-1",
	}
	projectID := testDefaultProjectID
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
		Pools:          stubs,
		Jobs:           stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodGet, "/projects/"+projectID+"/sandboxes/sandbox-1/git-repositories/primary.git/info/refs?service=git-receive-pack", nil, poolagentauth.ScopeSandboxWrite)
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("GET git receive info/refs status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if !slices.Equal(stubs.sandboxScopes, []string{poolagentauth.ScopeSandboxWrite}) {
		t.Fatalf("sandbox HTTP scopes = %#v", stubs.sandboxScopes)
	}
}

func TestSandboxHTTPRouteProxiesPortToWorker(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       testDefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		PoolID:          "pool-1",
	}
	var released bool
	projectID := testDefaultProjectID
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/project/" + projectID + "/pool/pool-1/sandboxes/sandbox-1/http/8080/api/status"
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
		Pools:          stubs,
		Jobs:           stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodGet, "/projects/"+projectID+"/sandboxes/sandbox-1/http/8080/api/status?verbose=true", nil, poolagentauth.ScopeSandboxHTTP)
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
	if !slices.Equal(stubs.sandboxScopes, []string{poolagentauth.ScopeSandboxHTTP}) {
		t.Fatalf("sandbox HTTP scopes = %#v", stubs.sandboxScopes)
	}
}

func TestSandboxExecListRouteProxiesToSandboxAgent(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       testDefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		PoolID:          "pool-1",
	}
	var released bool
	projectID := testDefaultProjectID
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/project/" + projectID + "/pool/pool-1/sandboxes/sandbox-1/execs"
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
		Pools:          stubs,
		Jobs:           stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodGet, "/api/projects/"+projectID+"/sandboxes/sandbox-1/execs", nil, poolagentauth.ScopeExecRead)
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
	if !slices.Equal(stubs.sandboxScopes, []string{poolagentauth.ScopeExecRead}) {
		t.Fatalf("sandbox HTTP scopes = %#v", stubs.sandboxScopes)
	}
}

func TestSandboxExecProxyErrorUsesJSON(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       testDefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		PoolID:          "pool-1",
	}
	projectID := testDefaultProjectID

	router, err := NewRouter(services.Services{
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Pools:          stubs,
		Jobs:           stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodPost, "/api/projects/"+projectID+"/sandboxes/sandbox-1/execs", nil, poolagentauth.ScopeExecWrite)
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
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       testDefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		PoolID:          "pool-1",
	}
	projectID := testDefaultProjectID
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/project/" + projectID + "/pool/pool-1/sandboxes/sandbox-1/execs/exec-1/attach"
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
		Pools:          stubs,
		Jobs:           stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodPost, "/api/projects/"+projectID+"/sandboxes/sandbox-1/execs/exec-1/attach", nil, poolagentauth.ScopeExecWrite)
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusNoContent {
		t.Fatalf("POST sandbox agent terminal attach status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if !slices.Equal(stubs.sandboxScopes, []string{poolagentauth.ScopeExecWrite, poolagentauth.ScopeExecRead}) {
		t.Fatalf("sandbox HTTP scopes = %#v", stubs.sandboxScopes)
	}
}

func TestSandboxHarnessHookRouteUsesExecReadScope(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       testDefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		PoolID:          "pool-1",
	}
	projectID := testDefaultProjectID
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/project/" + projectID + "/pool/pool-1/sandboxes/sandbox-1/harness-hooks"
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
		Pools:          stubs,
		Jobs:           stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodGet, "/api/projects/"+projectID+"/sandboxes/sandbox-1/harness-hooks", nil, poolagentauth.ScopeExecRead)
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("GET harness hooks status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if !slices.Equal(stubs.sandboxScopes, []string{poolagentauth.ScopeExecRead}) {
		t.Fatalf("sandbox HTTP scopes = %#v", stubs.sandboxScopes)
	}
}

func TestSandboxExecRoutesUseExecScopes(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       testDefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		PoolID:          "pool-1",
	}
	projectID := testDefaultProjectID
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/project/" + projectID + "/pool/pool-1/sandboxes/sandbox-1/execs"
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
		Pools:          stubs,
		Jobs:           stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodGet, "/api/projects/"+projectID+"/sandboxes/sandbox-1/execs", nil, poolagentauth.ScopeExecRead)
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("GET sandbox execs status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if !slices.Equal(stubs.sandboxScopes, []string{poolagentauth.ScopeExecRead}) {
		t.Fatalf("sandbox HTTP scopes = %#v", stubs.sandboxScopes)
	}
}

func TestSandboxExecAttachRouteUsesExecWriteScope(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       testDefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		PoolID:          "pool-1",
	}
	projectID := testDefaultProjectID
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/project/" + projectID + "/pool/pool-1/sandboxes/sandbox-1/execs/exec-1/attach"
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
		Pools:          stubs,
		Jobs:           stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodGet, "/api/projects/"+projectID+"/sandboxes/sandbox-1/execs/exec-1/attach", nil, poolagentauth.ScopeExecWrite)
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("GET sandbox exec attach status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if !slices.Equal(stubs.sandboxScopes, []string{poolagentauth.ScopeExecWrite, poolagentauth.ScopeExecRead}) {
		t.Fatalf("sandbox HTTP scopes = %#v", stubs.sandboxScopes)
	}
}

func TestNewAppStartsWithDefaults(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)

	router, _, _, _, err := NewApp(ctx, db.Write, db.Read)
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
	if !id.IsGenerated(body.Projects[0].ID) || !strings.HasPrefix(body.Projects[0].ID, id.PrefixProject+"_") {
		t.Fatalf("project ID = %q, want a generated proj_ id", body.Projects[0].ID)
	}
}

func TestNewAppResolvesDefaultProjectAlias(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)

	router, _, _, _, err := NewApp(ctx, db.Write, db.Read)
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
	if !id.IsGenerated(project.ID) || !strings.HasPrefix(project.ID, id.PrefixProject+"_") {
		t.Fatalf("project ID = %q, want a generated proj_ id", project.ID)
	}
	if !project.Default {
		t.Fatal("expected default project flag")
	}
}
