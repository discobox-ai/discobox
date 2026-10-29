package proxyagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy"
	"github.com/discobox-ai/discobox/proxy/gateway"
)

// A sandbox's recorded gateways reach the running proxy through the policy
// file, route that sandbox alone, and are gone with its policy.
func TestSandboxGatewaysRouteOnlyThatSandbox(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := withTestRoot(t)

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "provider")
	}))
	defer provider.Close()
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "gateway "+r.URL.Path+" "+r.Header.Get("X-Team"))
	}))
	defer gw.Close()

	bundle, err := PrepareBundle(root, testProjectID, testPoolID)
	if err != nil {
		t.Fatalf("prepare bundle: %v", err)
	}
	routed, err := proxy.EnsureClientCertificate(bundle, "sb-1", PoolProxyURL, "", time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	other, err := proxy.EnsureClientCertificate(bundle, "sb-2", PoolProxyURL, "", time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cfg := proxy.DefaultConfig()
	cfg.AllowLoopbackDestinations = true
	cfg.ListenAddress = "127.0.0.1:0"
	cfg.CertDir = bundle.Dir
	cfg.DatabaseDSN = filepath.Join(root.Dir(), testProjectID+"-audit.db")
	cfg.Recording.Enabled = false
	server, err := proxy.NewServer(ctx, cfg, bundle, nil)
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	policy := newPolicyPublisher(server, cfg, func(err error) { t.Errorf("apply proxy config: %v", err) })
	go func() { _ = server.ListenAndServe() }()
	routedClient := sandboxClient(t, server, routed)
	otherClient := sandboxClient(t, server, other)

	publish := func() {
		t.Helper()
		doc, err := readSecretsDoc(root.ProxySecretsFile(testProjectID, testPoolID))
		if err != nil {
			t.Fatal(err)
		}
		policy.setFile(doc)
	}
	route := gateway.Normalize(gateway.Route{
		Name:           "provider",
		Match:          provider.URL + "/v1",
		Target:         gw.URL + "/gw",
		RequestHeaders: gateway.HeaderModifier{Set: []gateway.Header{{Name: "X-Team", Value: "platform"}}},
	})
	if err := SetSandboxGateways(root, testProjectID, testPoolID, "sb-1", []gateway.Route{route}); err != nil {
		t.Fatal(err)
	}
	publish()

	if got := body(t, routedClient, provider.URL+"/v1/messages"); got != "gateway /gw/messages platform" {
		t.Fatalf("routed sandbox got %q", got)
	}
	if got := body(t, otherClient, provider.URL+"/v1/messages"); got != "provider" {
		t.Fatalf("another sandbox got %q", got)
	}

	if err := RemoveSandboxPolicy(root, testProjectID, testPoolID, "sb-1"); err != nil {
		t.Fatal(err)
	}
	publish()
	if got := body(t, routedClient, provider.URL+"/v1/messages"); got != "provider" {
		t.Fatalf("after its policy was removed the sandbox got %q", got)
	}
}

func body(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestResolverAsksForGatewayHeaders(t *testing.T) {
	root := withTestRoot(t)
	var gotAuth, gotPath string
	var gotBody gatewayHeadersBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		// Spelled as the server's answer spells it.
		_, _ = w.Write([]byte(`{"values":{"DISCOBOX_GATEWAY_KEY":"gw-key"}}`))
	}))
	defer srv.Close()
	if err := WriteResolveContext(root, testProjectID, testPoolID, srv.URL, "tok-123"); err != nil {
		t.Fatal(err)
	}
	resolver := newSecretResolver(root, testProjectID, testPoolID)
	values, err := resolver.ResolveGatewayHeaders(context.Background(), proxy.GatewayHeadersRequest{
		ClientID: "sb-1", Gateway: "anthropic", Env: []string{"DISCOBOX_GATEWAY_KEY", "DISCOBOX_GATEWAY_UNSET"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values["DISCOBOX_GATEWAY_KEY"] != "gw-key" {
		t.Fatalf("values = %v", values)
	}
	if gotAuth != "Bearer tok-123" || gotPath != "/api/pools/"+testPoolID+"/resolve-gateway-headers" {
		t.Fatalf("asked %s with %q", gotPath, gotAuth)
	}
	if gotBody.SandboxID != "sb-1" || gotBody.Gateway != "anthropic" || strings.Join(gotBody.Env, ",") != "DISCOBOX_GATEWAY_KEY,DISCOBOX_GATEWAY_UNSET" {
		t.Fatalf("body = %+v", gotBody)
	}
}

// Anything but an answer is no answer: the proxy holds the request.
func TestResolverGatewayHeadersRefusedIsAnError(t *testing.T) {
	root := withTestRoot(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	_ = WriteResolveContext(root, testProjectID, testPoolID, srv.URL, "tok")
	resolver := newSecretResolver(root, testProjectID, testPoolID)
	if _, err := resolver.ResolveGatewayHeaders(context.Background(), proxy.GatewayHeadersRequest{ClientID: "sb-1", Gateway: "g", Env: []string{"DISCOBOX_GATEWAY_X"}}); err == nil {
		t.Fatal("a 404 was taken as an answer")
	}
}
