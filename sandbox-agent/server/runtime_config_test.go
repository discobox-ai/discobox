package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/intake"
	"github.com/discobox-ai/discobox/sandboxconfig"
)

const runtimeConfigPath = "/api/projects/project-1/sandboxes/sandbox-1/runtime-config"

func runtimeConfigRouter(t *testing.T) (http.Handler, func(scopes ...string) string) {
	t.Helper()
	publicKey, signToken := sandboxAgentTestSigner(t)
	root := t.TempDir()
	in, err := intake.Open(intake.Layout{
		ConfigDir:   filepath.Join(root, "etc"),
		ProxyDir:    filepath.Join(root, "etc", "proxy"),
		SecretsPath: filepath.Join(root, "run", "secrets.json"),
		StatePath:   filepath.Join(root, "var", "runtime-config.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(publicKey)
	cfg.RuntimeConfig = in
	router, err := NewRouter(cfg)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	return router, func(scopes ...string) string { return signToken("project-1", "sandbox-1", "worker-1", scopes...) }
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
	bridge := func(prefix string) *sandboxconfig.RuntimeBridge {
		return &sandboxconfig.RuntimeBridge{
			ListenAddress:    prefix + "-listen",
			UpstreamURL:      prefix + "-upstream",
			CredentialsURL:   prefix + "-credentials",
			DNSServer:        prefix + "-dns",
			DNSListenAddress: prefix + "-dns-listen",
		}
	}
	full := sandboxconfig.RuntimeConfig{
		Revision:  7,
		Agent:     sandboxconfig.RuntimeAgent{IdleTimeout: "1h0m0s"},
		SecretEnv: map[string]string{"A": "sentinel-a"},
		Proxy: &sandboxconfig.RuntimeProxy{
			MTLSCA:            "mtls",
			MITMCA:            "mitm",
			ClientCert:        "cert",
			ClientKey:         "key",
			Egress:            bridge("egress"),
			NestedDocker:      bridge("docker"),
			BuildKit:          bridge("buildkit"),
			RegistryNamespace: "ns",
		},
		Sources: []sandboxconfig.RuntimeSource{{Slug: "primary", OriginURL: "https://pool/o", Commit: "abc", Delivered: true}},
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
