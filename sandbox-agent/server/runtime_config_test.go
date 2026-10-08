package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/discobox-ai/discobox/platform"
	"github.com/discobox-ai/discobox/sandbox-agent/intake"
	"github.com/discobox-ai/discobox/sandboxconfig"
	"github.com/discobox-ai/discobox/sandboxpath"
)

const runtimeConfigPath = "/api/projects/project-1/sandboxes/sandbox-1/runtime-config"

func runtimeConfigRouter(t *testing.T) (http.Handler, func(scopes ...string) string) {
	t.Helper()
	router, token, _ := runtimeConfigRouterWithPool(t)
	return router, token
}

// runtimeConfigRouterWithPool is runtimeConfigRouter with the pool's key in the
// bootstrap, and returns a signer for it beside the control plane's.
func runtimeConfigRouterWithPool(t *testing.T) (http.Handler, func(scopes ...string) string, func(poolID string, scopes ...string) string) {
	t.Helper()
	publicKey, signToken := sandboxAgentTestSigner(t)
	poolKey, signPoolToken := sandboxAgentTestSigner(t)
	root := t.TempDir()
	in, err := intake.Open(context.Background(), intake.Config{
		Layout: intake.Layout{
			ConfigDir:   filepath.Join(root, "etc"),
			ProxyDir:    filepath.Join(root, "etc", "proxy"),
			SecretsPath: filepath.Join(root, "run", "secrets.json"),
			StatePath:   filepath.Join(root, "var", "runtime-config.json"),
			// The test host stands in for the sandbox: its targets are real
			// directories here, judged by this platform's rules.
			Paths: sandboxpath.For(platform.Current()),
		},
		Owner: intake.Owner{ProjectID: "project-1", SandboxID: "sandbox-1", PoolID: "worker-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(publicKey)
	cfg.PoolPublicKey = poolKey
	cfg.RuntimeConfig = in
	router, err := NewRouter(cfg)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	return router,
		func(scopes ...string) string { return signToken("project-1", "sandbox-1", "worker-1", scopes...) },
		func(poolID string, scopes ...string) string {
			return signPoolToken("project-1", "sandbox-1", poolID, scopes...)
		}
}

func serveRuntimeConfig(t *testing.T, router http.Handler, method, token string, doc *sandboxconfig.RuntimeConfig) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	if doc != nil {
		if err := json.NewEncoder(&body).Encode(doc); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequestWithContext(context.Background(), method, runtimeConfigPath, &body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

func decodeRuntimeConfig(t *testing.T, resp *httptest.ResponseRecorder) sandboxconfig.RuntimeConfig {
	t.Helper()
	var doc sandboxconfig.RuntimeConfig
	if err := json.Unmarshal(resp.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode %s: %v", resp.Body.String(), err)
	}
	return doc
}

func plainRuntimeConfig(revision int64, secret string) *sandboxconfig.RuntimeConfig {
	return &sandboxconfig.RuntimeConfig{
		Revision:  revision,
		Agent:     sandboxconfig.RuntimeAgent{IdleTimeout: "10m0s"},
		SecretEnv: map[string]string{"GH_TOKEN": secret},
		Sources:   []sandboxconfig.RuntimeSource{{Slug: "primary", Commit: "abc123", Delivered: true}},
	}
}

func TestRuntimeConfigIsDeliveredAppliedAndReported(t *testing.T) {
	router, token := runtimeConfigRouter(t)
	pool := token(ScopeRuntimeConfig)

	if resp := serveRuntimeConfig(t, router, http.MethodGet, pool, nil); resp.Code != http.StatusNotFound {
		t.Fatalf("GET before any delivery = %d %s, want 404", resp.Code, resp.Body.String())
	}

	resp := serveRuntimeConfig(t, router, http.MethodPut, pool, plainRuntimeConfig(2, "discobox-sentinel-2"))
	if resp.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", resp.Code, resp.Body.String())
	}
	if got := decodeRuntimeConfig(t, resp); !got.SameDocument(*plainRuntimeConfig(2, "discobox-sentinel-2")) {
		t.Fatalf("PUT answered %+v", got)
	}

	resp = serveRuntimeConfig(t, router, http.MethodGet, pool, nil)
	if resp.Code != http.StatusOK || decodeRuntimeConfig(t, resp).Revision != 2 {
		t.Fatalf("GET = %d %s", resp.Code, resp.Body.String())
	}

	// An older delivery is answered with what the sandbox holds.
	resp = serveRuntimeConfig(t, router, http.MethodPut, pool, plainRuntimeConfig(1, "discobox-sentinel-1"))
	if resp.Code != http.StatusOK || decodeRuntimeConfig(t, resp).Revision != 2 {
		t.Fatalf("older PUT = %d %s, want 200 holding revision 2", resp.Code, resp.Body.String())
	}

	resp = serveRuntimeConfig(t, router, http.MethodPut, pool, plainRuntimeConfig(2, "discobox-sentinel-other"))
	if resp.Code != http.StatusConflict {
		t.Fatalf("conflicting PUT = %d %s, want 409", resp.Code, resp.Body.String())
	}

	invalid := plainRuntimeConfig(3, "discobox-sentinel-3")
	invalid.Agent.IdleTimeout = "soon"
	if resp := serveRuntimeConfig(t, router, http.MethodPut, pool, invalid); resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid PUT = %d %s, want 422", resp.Code, resp.Body.String())
	}

	status := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/status", nil)
	req.Header.Set("Authorization", "Bearer "+token(ScopeStatusRead))
	router.ServeHTTP(status, req)
	var body struct {
		RuntimeConfigRevision int64 `json:"runtimeConfigRevision"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &body); err != nil || body.RuntimeConfigRevision != 2 {
		t.Fatalf("status runtimeConfigRevision = %d (%v), body %s", body.RuntimeConfigRevision, err, status.Body.String())
	}
}

// Every invalid document is the sandbox's to refuse, the same way: revision 0
// included, which the schema leaves to the intake's validation.
func TestRuntimeConfigRevisionZeroIsInvalid(t *testing.T) {
	router, token := runtimeConfigRouter(t)
	resp := serveRuntimeConfig(t, router, http.MethodPut, token(ScopeRuntimeConfig), plainRuntimeConfig(0, "discobox-sentinel-0"))
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PUT revision 0 = %d %s, want 422", resp.Code, resp.Body.String())
	}
}

func TestRuntimeConfigIsThePoolsAlone(t *testing.T) {
	router, token := runtimeConfigRouter(t)
	for name, scopes := range map[string][]string{
		"wildcard":    {"*"},
		"exec write":  {ScopeExecWrite, ScopeExecRead},
		"status read": {ScopeStatusRead},
	} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			resp := serveRuntimeConfig(t, router, method, token(scopes...), plainRuntimeConfig(1, "discobox-sentinel-1"))
			if resp.Code != http.StatusForbidden {
				t.Errorf("%s %s with %s = %d, want 403", method, runtimeConfigPath, name, resp.Code)
			}
		}
	}
}

// The pool signs the token that delivers a document with its own key, and that
// key authorizes the runtime-config route and nothing else (ADR 26-10-08-127 §3).
func TestRuntimeConfigTakesThePoolsOwnToken(t *testing.T) {
	router, _, poolToken := runtimeConfigRouterWithPool(t)
	resp := serveRuntimeConfig(t, router, http.MethodPut, poolToken("worker-1", ScopeRuntimeConfig), plainRuntimeConfig(1, "discobox-sentinel-1"))
	if resp.Code != http.StatusOK {
		t.Fatalf("PUT with the pool's token = %d %s", resp.Code, resp.Body.String())
	}
	for name, token := range map[string]string{
		"a second scope":  poolToken("worker-1", ScopeRuntimeConfig, ScopeExecRead),
		"a wildcard":      poolToken("worker-1", "*"),
		"another pool":    poolToken("worker-2", ScopeRuntimeConfig),
		"no pool claimed": poolToken("", ScopeRuntimeConfig),
	} {
		if resp := serveRuntimeConfig(t, router, http.MethodPut, token, plainRuntimeConfig(2, "discobox-sentinel-2")); resp.Code != http.StatusForbidden {
			t.Errorf("PUT with a pool token carrying %s = %d, want 403", name, resp.Code)
		}
	}
	// Not even the scope the control plane mints the pool may be signed by it.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/sandboxes/sandbox-1/status", nil)
	req.Header.Set("Authorization", "Bearer "+poolToken("worker-1", ScopeRuntimeConfig))
	status := httptest.NewRecorder()
	router.ServeHTTP(status, req)
	if status.Code != http.StatusForbidden {
		t.Fatalf("status with a pool-signed token = %d, want 403", status.Code)
	}
	// What the pool may reach is every route whose required scope is
	// runtime-config: a source's project layer, which it reads to settle the
	// spec, is one (this router has no converger, so it answers 503 once
	// authorized), and an exec route is not.
	for path, refused := range map[string]bool{
		"/api/projects/project-1/sandboxes/sandbox-1/sources/primary/project-layer": false,
		"/api/projects/project-1/sandboxes/sandbox-1/execs":                         true,
	} {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+poolToken("worker-1", ScopeRuntimeConfig))
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		if gotRefused := resp.Code == http.StatusForbidden || resp.Code == http.StatusUnauthorized; gotRefused != refused {
			t.Errorf("GET %s with a pool-signed token = %d, want refused=%v", path, resp.Code, refused)
		}
	}
}

// A sandbox whose bootstrap names no pool key trusts no pool-signed token.
func TestRuntimeConfigRefusesAPoolTokenWithoutAPoolKey(t *testing.T) {
	publicKey, _ := sandboxAgentTestSigner(t)
	_, signPoolToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	resp := serveRuntimeConfig(t, router, http.MethodPut, signPoolToken("project-1", "sandbox-1", "worker-1", ScopeRuntimeConfig), plainRuntimeConfig(1, "s"))
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("PUT with an unknown pool's token = %d, want 401", resp.Code)
	}
}

func TestRuntimeConfigWithoutAnIntake(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	resp := serveRuntimeConfig(t, router, http.MethodPut, signToken("project-1", "sandbox-1", "worker-1", ScopeRuntimeConfig), plainRuntimeConfig(1, "s"))
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT without an intake = %d %s, want 503", resp.Code, resp.Body.String())
	}
}

// The generated schema and sandboxconfig.RuntimeConfig are one wire shape. A
// document with every field set must come back through the generated type
// unchanged; a field added to one and not the other fails here.
func TestRuntimeConfigWireRoundTrip(t *testing.T) {
	full := sandboxconfig.RuntimeConfig{
		Revision:  7,
		Agent:     sandboxconfig.RuntimeAgent{IdleTimeout: "1h0m0s"},
		SecretEnv: map[string]string{"A": "sentinel-a"},
		Proxy: &sandboxconfig.RuntimeProxy{
			MTLSCA:            "mtls",
			MITMCA:            "mitm",
			ClientCert:        "cert",
			ClientKey:         "key",
			RegistryNamespace: "ns",
		},
		Sources: []sandboxconfig.RuntimeSource{{Slug: "primary", Target: "/workspace", OriginURL: "https://pool/o", OriginToken: "token", Commit: "abc", Delivered: true}},
	}
	assertEveryFieldSet(t, reflect.ValueOf(full), "RuntimeConfig")

	wire, err := runtimeConfigToWire(full)
	if err != nil {
		t.Fatal(err)
	}
	back, err := runtimeConfigFromWire(wire)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, full) {
		t.Fatalf("round trip lost fields:\n got %+v\nwant %+v", back, full)
	}
}

// assertEveryFieldSet fails on any zero field, so the round-trip fixture
// cannot fall behind the type it covers.
func assertEveryFieldSet(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			t.Errorf("%s is unset in the round-trip fixture", path)
			return
		}
		assertEveryFieldSet(t, v.Elem(), path)
	case reflect.Struct:
		for i := range v.NumField() {
			assertEveryFieldSet(t, v.Field(i), path+"."+v.Type().Field(i).Name)
		}
	case reflect.Slice:
		if v.Len() == 0 {
			t.Errorf("%s is unset in the round-trip fixture", path)
		}
		for i := range v.Len() {
			assertEveryFieldSet(t, v.Index(i), path)
		}
	default:
		if v.IsZero() {
			t.Errorf("%s is unset in the round-trip fixture", path)
		}
	}
}

func TestRuntimeConfigNeverAnswersWithTheClientKey(t *testing.T) {
	doc := sandboxconfig.RuntimeConfig{Revision: 1, Proxy: &sandboxconfig.RuntimeProxy{ClientCert: "cert", ClientKey: "key"}}
	wire, err := runtimeConfigToWire(intake.WithoutClientKey(doc))
	if err != nil {
		t.Fatal(err)
	}
	data, err := wire.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("clientKey")) {
		t.Fatalf("answered document carries the client key: %s", data)
	}
	if doc.Proxy.ClientKey != "key" {
		t.Fatal("redacting the answer changed the held document")
	}
}

// materializedCheckout is a source checkout carrying the materialized marker,
// as the converger leaves one, with the given project layer.
func materializedCheckout(t *testing.T, projectLayer string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--quiet")
	if projectLayer != "" {
		if err := os.MkdirAll(filepath.Join(dir, ".discobox"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".discobox", "project.json"), []byte(projectLayer), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "--allow-empty", "-m", "init")
	if err := os.WriteFile(filepath.Join(dir, ".git", sandboxconfig.SourceMaterializedMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func getPath(t *testing.T, router http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

func TestSourceProjectLayerIsReadForThePool(t *testing.T) {
	router, token := runtimeConfigRouter(t)
	pool := token(ScopeRuntimeConfig)
	withLayer := materializedCheckout(t, `{"runCommand":["make"]}`)
	without := materializedCheckout(t, "")
	pending := t.TempDir()
	doc := &sandboxconfig.RuntimeConfig{Revision: 1, Sources: []sandboxconfig.RuntimeSource{
		{Slug: "primary", Target: withLayer, OriginURL: "https://pool/primary.git"},
		{Slug: "execs", Target: without, OriginURL: "https://pool/execs.git"},
		{Slug: "pending", Target: pending, OriginURL: "https://pool/pending.git"},
	}}
	if resp := serveRuntimeConfig(t, router, http.MethodPut, pool, doc); resp.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", resp.Code, resp.Body.String())
	}
	layerPath := func(slug string) string {
		return "/api/projects/project-1/sandboxes/sandbox-1/sources/" + slug + "/project-layer"
	}

	resp := getPath(t, router, layerPath("primary"), pool)
	var got struct {
		Slug         string         `json:"slug"`
		Commit       string         `json:"commit"`
		ProjectLayer map[string]any `json:"projectLayer"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &got); resp.Code != http.StatusOK || err != nil || got.Slug != "primary" || len(got.Commit) != 40 || got.ProjectLayer["runCommand"] == nil {
		t.Fatalf("GET primary = %d %s", resp.Code, resp.Body.String())
	}
	// A slug is a path segment, and one that spells another route's name is
	// still this route, on this scope.
	resp = getPath(t, router, layerPath("execs"), pool)
	if resp.Code != http.StatusOK || bytes.Contains(resp.Body.Bytes(), []byte("projectLayer")) {
		t.Fatalf("GET a source with no project layer = %d %s, want 200 and none", resp.Code, resp.Body.String())
	}
	if resp := getPath(t, router, layerPath("pending"), pool); resp.Code != http.StatusConflict {
		t.Fatalf("GET an unmaterialized source = %d %s, want 409", resp.Code, resp.Body.String())
	}
	if resp := getPath(t, router, layerPath("nope"), pool); resp.Code != http.StatusNotFound {
		t.Fatalf("GET an unknown source = %d %s, want 404", resp.Code, resp.Body.String())
	}
	for name, scopes := range map[string][]string{
		"wildcard":        {"*"},
		"exec read":       {ScopeExecRead, ScopeExecWrite},
		"status":          {ScopeStatusRead},
		"no scope at all": {},
	} {
		for _, slug := range []string{"primary", "execs"} {
			if resp := getPath(t, router, layerPath(slug), token(scopes...)); resp.Code != http.StatusForbidden {
				t.Errorf("GET %s with %s = %d, want 403", slug, name, resp.Code)
			}
		}
	}

	// The same sources ride the status poll, in the document's order.
	status := getPath(t, router, "/api/projects/project-1/sandboxes/sandbox-1/status", token(ScopeStatusRead))
	var body struct {
		SourceStates []struct {
			Slug     string `json:"slug"`
			State    string `json:"state"`
			Revision int64  `json:"revision"`
		} `json:"sourceStates"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &body); err != nil || len(body.SourceStates) != 3 ||
		body.SourceStates[0].Slug != "primary" || body.SourceStates[0].State != "waiting" || body.SourceStates[0].Revision != 1 {
		t.Fatalf("status sourceStates = %+v (%v), body %s", body.SourceStates, err, status.Body.String())
	}
}
