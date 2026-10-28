package proxyagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/proxy"
)

// The sandbox reads its raw secrets from the credentials endpoint over its own
// certificate (ADR 26-10-27-840): each raw binding SecretsFile names for it,
// in its variable and delivery, with the value the control plane hands out
// for its sentinel. One the control plane denies is left out, and the
// environment read names a file-delivered one by its path and never a value.
func TestCredentialsEndpointHandsTheSandboxItsRawSecrets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := withTestRoot(t)
	expires := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	var mu sync.Mutex
	var asked []resolveRawRequestBody
	var tokens []string
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			return // the sandbox agent's port probe, inside a discobox; see REVIEW.md
		}
		if r.URL.Path != "/api/pools/"+testPoolID+"/resolve-sandbox-raw-secret" {
			http.NotFound(w, r)
			return
		}
		var body resolveRawRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		asked = append(asked, body)
		tokens = append(tokens, r.Header.Get("Authorization"))
		mu.Unlock()
		switch body.Sentinel {
		case "RAW-FILE":
			_ = json.NewEncoder(w).Encode(resolveRawResponseBody{Status: "approved", Value: "file-secret-value-0001", ExpiresAt: &expires, SecretID: "sec_f", BindingID: "sbd_f"})
		case "RAW-ENV":
			_ = json.NewEncoder(w).Encode(resolveRawResponseBody{Status: "approved", Value: "env-secret-value-00002", SecretID: "sec_e", BindingID: "sbd_e"})
		default:
			_ = json.NewEncoder(w).Encode(resolveRawResponseBody{Status: "denied"})
		}
	}))
	defer controlPlane.Close()
	if err := WriteResolveContext(root, testProjectID, testPoolID, controlPlane.URL, "tok"); err != nil {
		t.Fatalf("write resolve context: %v", err)
	}
	raw := map[string]RawSentinel{
		"RAW-FILE":    {EnvName: "AWS_SECRET_ACCESS_KEY", Delivery: sandboxconfig.RawDeliveryFile},
		"RAW-ENV":     {EnvName: "DB_PASSWORD", Delivery: sandboxconfig.RawDeliveryEnv},
		"RAW-REVOKED": {EnvName: "GONE", Delivery: sandboxconfig.RawDeliveryEnv},
	}
	if err := UpsertSandboxSentinels(root, testProjectID, testPoolID, "sb-1", nil, raw,
		map[string][][]secretroute.Route{"RAW-FILE": anyHost, "RAW-ENV": anyHost, "RAW-REVOKED": anyHost}); err != nil {
		t.Fatalf("register raw sentinels: %v", err)
	}
	if err := SetSandboxEnvironment(root, testProjectID, testPoolID, "sb-1", map[string]string{"GH_TOKEN": "ghp_SENTINEL"}); err != nil {
		t.Fatalf("set environment: %v", err)
	}

	bundle, err := PrepareBundle(root, testProjectID, testPoolID)
	if err != nil {
		t.Fatalf("prepare bundle: %v", err)
	}
	material, err := proxy.EnsureClientCertificate(bundle, "sb-1", PoolProxyURL, "", time.Hour, time.Minute)
	if err != nil {
		t.Fatalf("issue client certificate: %v", err)
	}
	cfg := proxy.DefaultConfig()
	cfg.ListenAddress = "127.0.0.1:0"
	cfg.CertDir = bundle.Dir
	cfg.DatabaseDSN = filepath.Join(root.Dir(), testProjectID+"-audit.db")
	cfg.Recording.Enabled = false
	resolver := newSecretResolver(root, testProjectID, testPoolID)
	server, err := proxy.NewServer(ctx, cfg, bundle, resolver)
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	secretsPath := root.ProxySecretsFile(testProjectID, testPoolID)
	policy := newPolicyPublisher(server, cfg, func(err error) { t.Errorf("apply proxy config: %v", err) })
	if err := policy.loadFile(secretsPath, resolver); err != nil {
		t.Fatalf("load policy file: %v", err)
	}

	listener := listenLocal(t)
	go func() {
		_ = serveCredentialsOn(ctx, testLogger(), listener, bundle, newControlPlaneCredentials(root, testProjectID, testPoolID), secretsPath, nil, server)
	}()
	client := mtlsClient(t, bundle, material)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+listener.Addr().String()+sandboxconfig.RawSecretsRoute, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("read raw secrets: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status = %d, cache-control %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	var got sandboxconfig.RawSecretsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := sandboxconfig.RawSecretsResponse{Secrets: []sandboxconfig.RawSecret{
		{Name: "AWS_SECRET_ACCESS_KEY", Delivery: sandboxconfig.RawDeliveryFile, Value: "file-secret-value-0001", ExpiresAt: &expires},
		{Name: "DB_PASSWORD", Delivery: sandboxconfig.RawDeliveryEnv, Value: "env-secret-value-00002"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("raw secrets = %+v, want %+v", got, want)
	}
	mu.Lock()
	for i, body := range asked {
		if body.SandboxID != "sb-1" || tokens[i] != "Bearer tok" {
			t.Errorf("control plane asked %+v with %q, want the certificate's sandbox and the pool's token", body, tokens[i])
		}
	}
	mu.Unlock()

	env, err := (&credentialBroker{sandboxID: "sb-1", secretsPath: secretsPath}).Environment(ctx)
	if err != nil {
		t.Fatalf("environment: %v", err)
	}
	wantEnv := map[string]string{
		"GH_TOKEN":              "ghp_SENTINEL",
		"AWS_SECRET_ACCESS_KEY": sandboxconfig.RawSecretPath("AWS_SECRET_ACCESS_KEY"),
	}
	if !reflect.DeepEqual(env, wantEnv) {
		t.Fatalf("environment = %v, want %v", env, wantEnv)
	}
}

// ResolveRaw tells the control plane's answer about the binding from an
// answer about the control plane: only the first is a denial.
func TestResolveRawTellsADenialFromAnOutage(t *testing.T) {
	for _, tc := range []struct {
		status int
		denied bool
	}{
		{status: http.StatusNotFound, denied: true},
		{status: http.StatusForbidden, denied: true},
		{status: http.StatusServiceUnavailable},
		{status: http.StatusTooManyRequests},
	} {
		root := withTestRoot(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
		}))
		_ = WriteResolveContext(root, testProjectID, testPoolID, srv.URL, "tok")
		_, err := newSecretResolver(root, testProjectID, testPoolID).ResolveRaw(context.Background(), proxy.SecretRawRequest{ClientID: "sb-1", Sentinel: "RAW"})
		srv.Close()
		if err == nil {
			t.Fatalf("%d: no error", tc.status)
		}
		if denied := errors.Is(err, proxy.ErrSecretResolveDenied); denied != tc.denied {
			t.Errorf("%d: denied = %v (%v), want %v", tc.status, denied, err, tc.denied)
		}
	}
}
