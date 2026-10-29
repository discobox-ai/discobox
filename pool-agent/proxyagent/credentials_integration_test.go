package proxyagent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/proxy"
)

// These drive the parts the unit tests cannot reach: a real proxy swapping a
// sentinel out of real traffic and holding it to its secret's routes, and a
// real mTLS listener deciding who the caller is from a certificate.

// startRoutedProxy runs the pool proxy over the resolver this package builds,
// with sentinel registered for sb-1 under routes, as SecretsFile would.
func startRoutedProxy(ctx context.Context, t *testing.T, controlPlaneURL, sentinel string, lists [][]secretroute.Route) (*proxy.Server, proxy.ClientMaterial) {
	t.Helper()
	root := withTestRoot(t)
	if err := WriteResolveContext(root, testProjectID, testPoolID, controlPlaneURL, "tok"); err != nil {
		t.Fatalf("write resolve context: %v", err)
	}
	bundle, err := PrepareBundle(root, testProjectID, testPoolID)
	if err != nil {
		t.Fatalf("prepare bundle: %v", err)
	}
	material, err := proxy.EnsureClientCertificate(bundle, "sb-1", PoolProxyURL, "", time.Hour, time.Minute)
	if err != nil {
		t.Fatalf("issue client certificate: %v", err)
	}
	if err := UpsertSandboxSentinels(root, testProjectID, testPoolID, "sb-1", []string{sentinel}, map[string][][]secretroute.Route{sentinel: lists}); err != nil {
		t.Fatalf("register sentinel: %v", err)
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
	policy := newPolicyPublisher(server, cfg, func(err error) { t.Errorf("apply proxy config: %v", err) })
	if err := policy.loadFile(root.ProxySecretsFile(testProjectID, testPoolID), resolver); err != nil {
		t.Fatalf("load policy file: %v", err)
	}
	go func() { _ = server.ListenAndServe() }()
	return server, material
}

// A sentinel is swapped on a request its secret's routes allow, and a request
// they refuse is answered by the proxy, never sent, and resolves nothing
// (ADR 26-10-02-924, ADR 26-10-27-880).
func TestASentinelIsHeldToItsRoutesOnRealTraffic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "ghp_SENTINELSENTINELSENTINELSENTINEL1"
	const realValue = "ghp_REALREALREALREALREALREALREALREAL12"

	var sawAuthorization []string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			return // the sandbox agent's port probe, inside a discobox; see REVIEW.md
		}
		sawAuthorization = append(sawAuthorization, r.URL.Path+" "+r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, "ok")
	}))
	defer origin.Close()
	originHost := mustHostname(t, origin.URL)

	var resolves int
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			return // the sandbox agent's port probe, inside a discobox; see REVIEW.md
		}
		var body resolveRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		resolves++
		if body.Sentinel != sentinel || body.SandboxID != "sb-1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		expiry := time.Now().Add(time.Hour)
		_ = json.NewEncoder(w).Encode(resolveResponseBody{Status: "approved", Value: realValue, ExpiresAt: &expiry})
	}))
	defer controlPlane.Close()

	server, material := startRoutedProxy(ctx, t, controlPlane.URL, sentinel, [][]secretroute.Route{{
		{Hosts: []string{originHost}},
		{Hosts: []string{originHost}, Paths: []string{"/admin/**"}, Blocked: true},
	}})
	client := proxyClient(t, server, material)
	get := func(path string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+sentinel)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("proxied request: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if status := get("/repos"); status != http.StatusOK {
		t.Fatalf("allowed request = %d, want the upstream's 200", status)
	}
	if status := get("/admin/users"); status != http.StatusForbidden {
		t.Fatalf("blocked route = %d, want the proxy's 403", status)
	}
	if len(sawAuthorization) != 1 || sawAuthorization[0] != "/repos Bearer "+realValue {
		t.Fatalf("upstream saw %q, want only the allowed request, with the real credential", sawAuthorization)
	}
	if resolves != 1 {
		t.Fatalf("resolved %d times, want once: a refused request resolves nothing", resolves)
	}
}

// TestCredentialsEndpointIdentifiesTheSandboxByItsCertificate serves the real
// mTLS endpoint and proves the two things it exists to guarantee: a caller is
// whoever its certificate says, and it cannot claim to be another sandbox.
func TestCredentialsEndpointIdentifiesTheSandboxByItsCertificate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := withTestRoot(t)

	var sawSandboxIDs []string
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			return // the sandbox agent's port probe, inside a discobox; see REVIEW.md
		}
		sawSandboxIDs = append(sawSandboxIDs, r.URL.Query().Get("sandboxId"))
		_ = json.NewEncoder(w).Encode(listCredentialsDoc{Credentials: []credentialDoc{{
			Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com"},
			SecretID: "sec-1", GrantID: "grant-1",
		}}})
	}))
	defer controlPlane.Close()
	if err := WriteResolveContext(root, testProjectID, testPoolID, controlPlane.URL, "tok"); err != nil {
		t.Fatalf("write resolve context: %v", err)
	}

	bundle, err := PrepareBundle(root, testProjectID, testPoolID)
	if err != nil {
		t.Fatalf("prepare bundle: %v", err)
	}
	material, err := proxy.EnsureClientCertificate(bundle, "sb-1", PoolProxyURL, "", time.Hour, time.Minute)
	if err != nil {
		t.Fatalf("issue client certificate: %v", err)
	}

	listener := listenLocal(t)
	go func() {
		_ = serveCredentialsOn(ctx, testLogger(), listener, bundle, newControlPlaneCredentials(root, testProjectID, testPoolID), root.ProxySecretsFile(testProjectID, testPoolID), nil, nil)
	}()

	client := agentcreds.NewClient("https://"+listener.Addr().String(), agentcreds.WithHTTPClient(mtlsClient(t, bundle, material)))
	credentials, err := client.List(ctx)
	if err != nil {
		t.Fatalf("list over mTLS: %v", err)
	}
	if len(credentials) != 1 || credentials[0].EnvVar != "GITHUB_TOKEN" {
		t.Fatalf("credentials = %#v", credentials)
	}
	if len(sawSandboxIDs) != 1 || sawSandboxIDs[0] != "sb-1" {
		t.Fatalf("control plane was asked about %v, want the certificate's sandbox", sawSandboxIDs)
	}
}

// TestCredentialsEndpointRefusesAnUnknownCertificate proves the listener is
// the authorization boundary: without a certificate the CA signed, there is no
// caller at all.
func TestCredentialsEndpointRefusesAnUnknownCertificate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := withTestRoot(t)

	bundle, err := PrepareBundle(root, testProjectID, testPoolID)
	if err != nil {
		t.Fatalf("prepare bundle: %v", err)
	}
	listener := listenLocal(t)
	go func() {
		_ = serveCredentialsOn(ctx, testLogger(), listener, bundle, newControlPlaneCredentials(root, testProjectID, testPoolID), root.ProxySecretsFile(testProjectID, testPoolID), nil, nil)
	}()

	// A client that trusts the server but presents nothing of its own.
	caPEM, err := os.ReadFile(bundle.MTLSCAPath)
	if err != nil {
		t.Fatalf("read CA: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	anonymous := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    pool,
		ServerName: ServerName,
		MinVersion: tls.VersionTLS12,
	}}, Timeout: 5 * time.Second}

	client := agentcreds.NewClient("https://"+listener.Addr().String(), agentcreds.WithHTTPClient(anonymous))
	if _, err := client.List(ctx); err == nil {
		t.Fatal("an unauthenticated caller listed credentials")
	}
}

func mustHostname(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return parsed.Hostname()
}

func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

// mtlsClient dials the pool endpoint the way a sandbox's relay does: the
// server's own CA, and this sandbox's client keypair.
func mtlsClient(t *testing.T, bundle *proxy.CertificateBundle, material proxy.ClientMaterial) *http.Client {
	t.Helper()
	caPEM, err := os.ReadFile(bundle.MTLSCAPath)
	if err != nil {
		t.Fatalf("read CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("parse CA")
	}
	cert, err := tls.LoadX509KeyPair(material.ClientCertPath, material.ClientKeyPath)
	if err != nil {
		t.Fatalf("load client keypair: %v", err)
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			TLSClientConfig: &tls.Config{
				RootCAs:      pool,
				Certificates: []tls.Certificate{cert},
				// The endpoint presents the pool proxy's name, which a sandbox
				// resolves through Docker DNS; here the address is loopback, so
				// the name has to be asserted rather than looked up.
				ServerName: ServerName,
				MinVersion: tls.VersionTLS12,
			},
		},
	}
}

// proxyClient routes requests through the running pool proxy over mTLS, the
// way the sandbox's egress bridge does.
func proxyClient(t *testing.T, server *proxy.Server, material proxy.ClientMaterial) *http.Client {
	t.Helper()
	addr := waitForProxyAddr(t, server)
	cert, err := tls.LoadX509KeyPair(material.ClientCertPath, material.ClientKeyPath)
	if err != nil {
		t.Fatalf("load client keypair: %v", err)
	}
	caPEM, err := os.ReadFile(material.MTLSCAPath)
	if err != nil {
		t.Fatalf("read CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("parse CA")
	}
	proxyURL, err := url.Parse("https://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{
				RootCAs:      pool,
				Certificates: []tls.Certificate{cert},
				ServerName:   "127.0.0.1",
				MinVersion:   tls.VersionTLS12,
			},
		},
	}
}

func waitForProxyAddr(t *testing.T, server *proxy.Server) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr := server.Addr(); addr != nil {
			return addr.String()
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("proxy never reported a listen address")
	return ""
}
