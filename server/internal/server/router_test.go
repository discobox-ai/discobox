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

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/sourcecache"
	"github.com/discobox-ai/discobox/server/internal/auth"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/service"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/transport"
	"github.com/discobox-ai/x/gormdb"
	"github.com/discobox-ai/x/id"
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

// newTestApp starts a real app on db and stops it when the test ends. NewApp
// starts background services — the reconcile engine, the provider manager's
// Docker watchers — and they outlive the test unless stopped, which leaves them
// querying a closed database and recreating the SQLite file inside the TempDir
// Go is deleting.
func newTestApp(ctx context.Context, t *testing.T, db *database.DB) *chi.Mux {
	t.Helper()
	router, _, _, stop, err := NewApp(ctx, db.Write, db.Read)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := stop(stopCtx); err != nil {
			t.Errorf("stop services: %v", err)
		}
	})
	return router
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

// A cache-delivered source's origin is its namespace of the pool's Git cache,
// which the control plane names when it forwards: the pool's cache route, the
// cache's key, and — for a discobox delivering to another — the caller.
// None of that is ever the client's word (ADR 26-10-10-545 §§3, 7).
func TestSandboxGitOriginRouteNamesTheCacheOfACacheDeliveredSource(t *testing.T) {
	ctx := context.Background()
	stubs := newRouterTestServices()
	slug, key := "primary", "remote/github.com/acme/widget"
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID:              "sandbox-1",
		ProjectID:       testDefaultProjectID,
		CreatedByUserID: service.DefaultUserID,
		Name:            "sandbox",
		PoolID:          "pool-1",
		SandboxManifest: model.SandboxManifest{
			Source: &model.GitSource{Kind: "git", Slug: &slug, Delivery: model.GitSourceDeliveryCache, CacheKey: &key},
		},
	}
	projectID := testDefaultProjectID
	type seen struct{ path, key, caller string }
	var got seen
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = seen{path: r.URL.Path, key: r.Header.Get(sourcecache.KeyHeader), caller: r.Header.Get(sourcecache.CallerHeader)}
		_, _ = w.Write([]byte("ok"))
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
	base := "/projects/" + projectID + "/sandboxes/sandbox-1/"
	poolBase := "/api/project/" + projectID + "/pool/pool-1/sandboxes/sandbox-1/"
	for name, tc := range map[string]struct {
		method, path, scope string
		want                seen
	}{
		"a push": {
			http.MethodPost, "git-origins/primary.git/git-receive-pack", poolagentauth.ScopeSandboxWrite,
			seen{path: poolBase + "git-cache-origins/primary.git/git-receive-pack", key: key},
		},
		"a pin": {
			http.MethodPost, "git-origins/primary.git/pin", poolagentauth.ScopeSandboxWrite,
			seen{path: poolBase + "git-cache-origins/primary.git/pin", key: key},
		},
		"a fetch of what the cache remembers of the remote": {
			http.MethodGet, "git-caches/primary.git/info/refs", poolagentauth.ScopeSandboxRead,
			seen{path: poolBase + "git-cache-remotes/primary.git/info/refs", key: key},
		},
	} {
		got = seen{}
		stubs.sandboxScopes = nil
		resp := httptest.NewRecorder()
		req := scopedUserRequest(ctx, tc.method, base+tc.path, nil, tc.scope)
		req.Header.Set("Authorization", "Bearer user-token")
		// A client that names a cache or a caller of its own is not believed.
		req.Header.Set(sourcecache.KeyHeader, "remote/github.com/someone/else")
		req.Header.Set(sourcecache.CallerHeader, "sbx-forged")
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body = %s", name, resp.Code, resp.Body.String())
		}
		if got != tc.want {
			t.Errorf("%s: the pool was asked %+v, want %+v", name, got, tc.want)
		}
		if !slices.Equal(stubs.sandboxScopes, []string{tc.scope}) {
			t.Errorf("%s: scopes = %v, want %s", name, stubs.sandboxScopes, tc.scope)
		}
	}

	// A discobox delivering to another is named to the pool, which
	// holds it to what it sends and what its own sources hold.
	got = seen{}
	asSandbox := httptest.NewRequestWithContext(ctx, http.MethodPost, base+"git-origins/primary.git/git-receive-pack", nil)
	asSandbox = asSandbox.WithContext(auth.WithPrincipal(asSandbox.Context(), auth.Principal{
		Type: auth.PrincipalTypeSandbox, SandboxID: "sbx-lead", ProjectID: projectID, UserID: service.DefaultUserID,
		Scopes: []string{poolagentauth.ScopeSandboxWrite},
	}))
	asSandbox.Header.Set(sourcecache.CallerHeader, "sbx-forged")
	router.ServeHTTP(httptest.NewRecorder(), asSandbox)
	if want := (seen{path: poolBase + "git-cache-origins/primary.git/git-receive-pack", key: key, caller: "sbx-lead"}); got != want {
		t.Errorf("a sandbox's push: the pool was asked %+v, want %+v", got, want)
	}

	// A source that is not cache-delivered has nothing on the cache route.
	resp := httptest.NewRecorder()
	req := scopedUserRequest(ctx, http.MethodGet, base+"git-caches/other.git/info/refs", nil, poolagentauth.ScopeSandboxRead)
	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("the cache route for a source with no cache = %d, want 404", resp.Code)
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

// Every route onto a sandbox's agent names what the request is for, and that
// is the one table of which of them starts a stopped sandbox and which may
// repair one (ADR 26-10-12-711 §1). A route missing here is one nobody
// classified.
func TestSandboxAgentRoutesNameTheirUse(t *testing.T) {
	stubs := newRouterTestServices()
	stubs.sandboxes["sandbox-1"] = model.Sandbox{
		ID: "sandbox-1", ProjectID: testDefaultProjectID,
		CreatedByUserID: service.DefaultUserID, PoolID: "pool-1",
	}
	var upstreamQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(upstream.Close)
	stubs.sandboxLease = transport.NewHTTPClientLeaseWithBaseURLAndAuth(upstream.Client(), upstream.URL, "worker-token", nil)
	router, err := NewRouter(services.Services{
		Projects: stubs, HarnessConfigs: stubs, Sandboxes: stubs,
		Providers: stubs, Pools: stubs, Jobs: stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	action := func(trigger string) services.SandboxUse { return services.SandboxAction(trigger) }
	attach := action("exec attach")
	attach.Attach = true
	base := "/api/projects/" + testDefaultProjectID + "/sandboxes/sandbox-1"
	for _, tc := range []struct {
		method, path string
		want         services.SandboxUse
	}{
		{http.MethodGet, "/harness-hooks", services.SandboxObserve},
		{http.MethodGet, "/exec-events", services.SandboxObserve},
		{http.MethodGet, "/execs/e1/screen", services.SandboxObserve},
		{http.MethodPost, "/execs/e1/wait", services.SandboxObserve},
		{http.MethodGet, "/execs", services.SandboxInspect},
		{http.MethodGet, "/execs/e1", services.SandboxInspect},
		{http.MethodGet, "/execs/e1/logs", services.SandboxRead},
		{http.MethodGet, "/execs/e1/events", services.SandboxRead},
		{http.MethodGet, "/execs/e1/resources", services.SandboxRead},
		{http.MethodGet, "/execs/e1/resources/history", services.SandboxRead},
		{http.MethodGet, "/execs/e1/resources/stream", services.SandboxRead},
		{http.MethodGet, "/services", services.SandboxInspect},
		{http.MethodGet, "/services/web", services.SandboxInspect},
		{http.MethodGet, "/services/web/logs", services.SandboxRead},
		{http.MethodGet, "/tools", services.SandboxInspect},
		{http.MethodPost, "/execs", action("exec create")},
		// The wait option predates every action waiting; it is still taken,
		// and still not forwarded.
		{http.MethodPost, "/execs?wait=ready", action("exec create")},
		{http.MethodDelete, "/execs/e1", action("exec delete")},
		{http.MethodPost, "/execs/e1/input", action("exec input")},
		{http.MethodPost, "/execs/e1/start", action("exec start")},
		{http.MethodPost, "/execs/e1/attach", attach},
		{http.MethodGet, "/execs/e1/attach", attach},
		{http.MethodPost, "/services/web/start", action("service start")},
		{http.MethodPost, "/services/web/stop", action("service stop")},
		{http.MethodPost, "/services/web/restart", action("service restart")},
		// An id spelled like an operation is still an id: the use is the
		// route's, not the path's last word.
		{http.MethodGet, "/services/attach", services.SandboxInspect},
		{http.MethodGet, "/services/wait", services.SandboxInspect},
		{http.MethodGet, "/execs/screen", services.SandboxInspect},
		{http.MethodDelete, "/execs/attach", action("exec delete")},
	} {
		stubs.sandboxUses = nil
		resp := httptest.NewRecorder()
		req := scopedUserRequest(context.Background(), tc.method, base+tc.path, nil, poolagentauth.ScopeExecWrite, poolagentauth.ScopeExecRead)
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusCreated {
			t.Errorf("%s %s: status = %d, body = %s", tc.method, tc.path, resp.Code, resp.Body.String())
			continue
		}
		if len(stubs.sandboxUses) != 1 || stubs.sandboxUses[0] != tc.want {
			t.Errorf("%s %s: acquired as %+v, want %+v", tc.method, tc.path, stubs.sandboxUses, tc.want)
		}
		if upstreamQuery != "" {
			t.Errorf("%s %s: upstream query = %q, want this server's wait option taken and not forwarded", tc.method, tc.path, upstreamQuery)
		}
	}

	// `wait=ready` is the one value of the option this server takes. Any
	// other is not its to accept, and reaches the sandbox agent to be refused
	// there rather than being dropped on the way.
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, scopedUserRequest(context.Background(), http.MethodPost, base+"/execs?wait=bogus", nil, poolagentauth.ScopeExecWrite))
	if upstreamQuery != "wait=bogus" {
		t.Errorf("POST /execs?wait=bogus: upstream query = %q, want the unknown value forwarded", upstreamQuery)
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

// Both audit trails a sandbox agent keeps — harness hooks and the events of
// every exec — are exec reads.
func TestSandboxAuditTrailRoutesUseExecReadScope(t *testing.T) {
	for _, trail := range []string{"harness-hooks", "exec-events"} {
		t.Run(trail, func(t *testing.T) {
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
				wantPath := "/api/project/" + projectID + "/pool/pool-1/sandboxes/sandbox-1/" + trail
				if r.URL.Path != wantPath {
					t.Errorf("upstream path = %q, want %q", r.URL.Path, wantPath)
				}
				_, _ = w.Write([]byte(`{}`))
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
			req := scopedUserRequest(ctx, http.MethodGet, "/api/projects/"+projectID+"/sandboxes/sandbox-1/"+trail+"?order=asc", nil, poolagentauth.ScopeExecRead)
			router.ServeHTTP(resp, req)

			if resp.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, body = %s", trail, resp.Code, resp.Body.String())
			}
			if !slices.Equal(stubs.sandboxScopes, []string{poolagentauth.ScopeExecRead}) {
				t.Fatalf("sandbox HTTP scopes = %#v", stubs.sandboxScopes)
			}
		})
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

// A terminal is read, typed into, and waited on without attaching (ADR 0137).
// A wait is a POST only because it carries a body, so it reads.
func TestSandboxExecTerminalRoutesUseExecScopes(t *testing.T) {
	for _, tc := range []struct {
		method string
		suffix string
		want   []string
	}{
		{http.MethodGet, "screen", []string{poolagentauth.ScopeExecRead}},
		{http.MethodPost, "input", []string{poolagentauth.ScopeExecWrite}},
		{http.MethodPost, "wait", []string{poolagentauth.ScopeExecRead}},
	} {
		t.Run(tc.suffix, func(t *testing.T) {
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
				wantPath := "/api/project/" + projectID + "/pool/pool-1/sandboxes/sandbox-1/execs/exec-1/" + tc.suffix
				if r.URL.Path != wantPath || r.Method != tc.method {
					t.Errorf("upstream = %s %q, want %s %q", r.Method, r.URL.Path, tc.method, wantPath)
				}
				_, _ = w.Write([]byte(`{}`))
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
			req := scopedUserRequest(ctx, tc.method, "/api/projects/"+projectID+"/sandboxes/sandbox-1/execs/exec-1/"+tc.suffix, nil, tc.want...)
			router.ServeHTTP(resp, req)

			if resp.Code != http.StatusOK {
				t.Fatalf("%s %s status = %d, body = %s", tc.method, tc.suffix, resp.Code, resp.Body.String())
			}
			if !slices.Equal(stubs.sandboxScopes, tc.want) {
				t.Fatalf("sandbox HTTP scopes = %#v, want %#v", stubs.sandboxScopes, tc.want)
			}
		})
	}
}

func TestNewAppStartsWithDefaults(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)

	router := newTestApp(ctx, t, db)

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

	router := newTestApp(ctx, t, db)

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
