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
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/agentcreds"
	"github.com/discobox-ai/discobox/proxy"
)

// These drive the parts the unit tests cannot reach: a real proxy swapping a
// freshly minted sentinel out of real traffic, and a real mTLS listener
// deciding who the caller is from a certificate.
//
// The swap test exists because that failure is silent. If an ephemeral
// sentinel never reaches the proxy's match set, nothing errors — the request
// simply goes out carrying the placeholder and the upstream rejects it, which
// looks like a credential problem rather than a wiring one.

// TestMintedSentinelIsSwappedOnRealTraffic runs the whole pool-side path:
// minting publishes to the proxy, the proxy matches the sentinel in an
// outbound request, and the resolver translates it back to the stable one
// before the control plane ever sees it.
func TestMintedSentinelIsSwappedOnRealTraffic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	withTestRoot(t)

	const realValue = "ghp_REALREALREALREALREALREALREALREAL12"

	var sawAuthorization string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthorization = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	}))
	defer origin.Close()
	originHost := mustHostname(t, origin.URL)

	// The control plane answers only for the stable sentinel, which is what
	// proves the translation happened rather than the ephemeral one leaking
	// through.
	var sawSentinel string
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body resolveRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		sawSentinel = body.Sentinel
		if body.Sentinel != "STABLE-SENTINEL" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		expiry := time.Now().Add(time.Hour)
		_ = json.NewEncoder(w).Encode(resolveResponseBody{Status: "approved", Value: realValue, ExpiresAt: &expiry})
	}))
	defer controlPlane.Close()
	if err := WriteResolveContext(testProjectID, testPoolID, controlPlane.URL, "tok"); err != nil {
		t.Fatalf("write resolve context: %v", err)
	}

	bundle, err := PrepareBundle(testProjectID, testPoolID)
	if err != nil {
		t.Fatalf("prepare bundle: %v", err)
	}
	material, err := proxy.EnsureClientCertificate(bundle, "sb-1", PoolProxyURL, "", time.Hour, time.Minute)
	if err != nil {
		t.Fatalf("issue client certificate: %v", err)
	}

	live := newActivations()
	cfg := proxy.DefaultConfig()
	cfg.ListenAddress = "127.0.0.1:0"
	cfg.CertDir = bundle.Dir
	cfg.DatabaseDSN = resolve(testProjectID + "-audit.db")
	cfg.Recording.Enabled = false
	server, err := proxy.NewServer(ctx, cfg, bundle, newSecretResolver(testProjectID, testPoolID, live))
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	newSentinelPublisher(server, cfg, live, func(err error) { t.Errorf("apply proxy config: %v", err) })
	go func() { _ = server.ListenAndServe() }()

	// Minting is what registers the sentinel. It happens before any request,
	// exactly as `get` does, and must take effect without waiting for a poll.
	record, err := live.mint("sb-1", "STABLE-SENTINEL", "use-1", originHost, "ghp_{base62:36}", []string{"gh", "pr", "create"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	client := proxyClient(t, server, material)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+record.Sentinel)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxied request: %v", err)
	}
	defer resp.Body.Close()

	if sawAuthorization != "Bearer "+realValue {
		t.Fatalf("upstream saw %q, want the real credential; the ephemeral sentinel was not swapped", sawAuthorization)
	}
	if sawSentinel != "STABLE-SENTINEL" {
		t.Fatalf("control plane saw %q, want the stable sentinel", sawSentinel)
	}
}

// TestMintedSentinelIsNotSwappedForAnotherHost is the same path with the
// destination the activation was not approved for. The sentinel must travel
// unchanged, so the upstream rejects a placeholder rather than receiving a
// credential it was never granted.
func TestMintedSentinelIsNotSwappedForAnotherHost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	withTestRoot(t)

	var sawAuthorization string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthorization = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	}))
	defer origin.Close()

	controlPlane := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("control plane was asked to resolve a sentinel used against an unapproved host")
	}))
	defer controlPlane.Close()
	if err := WriteResolveContext(testProjectID, testPoolID, controlPlane.URL, "tok"); err != nil {
		t.Fatalf("write resolve context: %v", err)
	}

	bundle, err := PrepareBundle(testProjectID, testPoolID)
	if err != nil {
		t.Fatalf("prepare bundle: %v", err)
	}
	material, err := proxy.EnsureClientCertificate(bundle, "sb-1", PoolProxyURL, "", time.Hour, time.Minute)
	if err != nil {
		t.Fatalf("issue client certificate: %v", err)
	}

	live := newActivations()
	cfg := proxy.DefaultConfig()
	cfg.ListenAddress = "127.0.0.1:0"
	cfg.CertDir = bundle.Dir
	cfg.DatabaseDSN = resolve(testProjectID + "-audit.db")
	cfg.Recording.Enabled = false
	server, err := proxy.NewServer(ctx, cfg, bundle, newSecretResolver(testProjectID, testPoolID, live))
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	newSentinelPublisher(server, cfg, live, func(err error) { t.Errorf("apply proxy config: %v", err) })
	go func() { _ = server.ListenAndServe() }()

	// Approved for somewhere the request is not going.
	record, err := live.mint("sb-1", "STABLE-SENTINEL", "use-1", "api.github.com", "ghp_{base62:36}", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	client := proxyClient(t, server, material)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+record.Sentinel)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxied request: %v", err)
	}
	defer resp.Body.Close()

	if sawAuthorization != "Bearer "+record.Sentinel {
		t.Fatalf("upstream saw %q, want the sentinel left in place", sawAuthorization)
	}
}

// TestCredentialsEndpointIdentifiesTheSandboxByItsCertificate serves the real
// mTLS endpoint and proves the two things it exists to guarantee: a caller is
// whoever its certificate says, and it cannot claim to be another sandbox.
func TestCredentialsEndpointIdentifiesTheSandboxByItsCertificate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	withTestRoot(t)

	var sawSandboxIDs []string
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawSandboxIDs = append(sawSandboxIDs, r.URL.Query().Get("sandboxId"))
		_ = json.NewEncoder(w).Encode(listCredentialsDoc{Credentials: []credentialDoc{{
			Name: "github", EnvVar: "GITHUB_TOKEN", Host: "api.github.com",
			SecretID: "sec-1", GrantID: "grant-1", Sentinel: "STABLE-SENTINEL",
			Format: "ghp_{base62:36}",
			Uses:   []credentialUseDoc{{UseID: "use-1", Description: "Open a PR"}},
		}}})
	}))
	defer controlPlane.Close()
	if err := WriteResolveContext(testProjectID, testPoolID, controlPlane.URL, "tok"); err != nil {
		t.Fatalf("write resolve context: %v", err)
	}

	bundle, err := PrepareBundle(testProjectID, testPoolID)
	if err != nil {
		t.Fatalf("prepare bundle: %v", err)
	}
	material, err := proxy.EnsureClientCertificate(bundle, "sb-1", PoolProxyURL, "", time.Hour, time.Minute)
	if err != nil {
		t.Fatalf("issue client certificate: %v", err)
	}

	live := newActivations()
	listener := listenLocal(t)
	go func() {
		_ = serveCredentialsOn(ctx, testLogger(), listener, bundle, testProjectID, testPoolID, live)
	}()

	client := agentcreds.NewClient("https://"+listener.Addr().String(), agentcreds.WithHTTPClient(mtlsClient(t, bundle, material)))

	credentials, err := client.List(ctx)
	if err != nil {
		t.Fatalf("list over mTLS: %v", err)
	}
	if len(credentials) != 1 || credentials[0].Uses[0].UseID != "use-1" {
		t.Fatalf("credentials = %#v", credentials)
	}
	// The stable sentinel is the one thing that would let a sandbox address the
	// credential directly, so it must not appear in what the sandbox is told.
	encoded, err := json.Marshal(credentials)
	if err != nil {
		t.Fatalf("marshal credentials: %v", err)
	}
	if strings.Contains(string(encoded), "STABLE-SENTINEL") {
		t.Fatalf("list leaked the stable sentinel to the sandbox: %s", encoded)
	}
	if len(sawSandboxIDs) == 0 || sawSandboxIDs[0] != "sb-1" {
		t.Fatalf("control plane was asked about %v, want the certificate's sandbox", sawSandboxIDs)
	}

	// A `get` mints against that identity, and the value handed back is an
	// ephemeral sentinel rather than anything the control plane holds.
	result, err := client.Get(ctx, agentcreds.UseBody{UseID: "use-1", Command: []string{"gh", "pr", "create"}})
	if err != nil {
		t.Fatalf("get over mTLS: %v", err)
	}
	if result.EnvVar != "GITHUB_TOKEN" || result.Value == "STABLE-SENTINEL" || !strings.HasPrefix(result.Value, "ghp_") {
		t.Fatalf("get returned %#v, want a freshly minted lookalike", result)
	}
	record, ok := live.lookup(result.Value)
	if !ok || record.SandboxID != "sb-1" || record.Stable != "STABLE-SENTINEL" {
		t.Fatalf("activation = %#v, want one bound to the certificate's sandbox", record)
	}
}

// TestCredentialsEndpointRefusesAnUnknownCertificate proves the listener is
// the authorization boundary: without a certificate the CA signed, there is no
// caller at all.
func TestCredentialsEndpointRefusesAnUnknownCertificate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	withTestRoot(t)

	bundle, err := PrepareBundle(testProjectID, testPoolID)
	if err != nil {
		t.Fatalf("prepare bundle: %v", err)
	}
	listener := listenLocal(t)
	go func() {
		_ = serveCredentialsOn(ctx, testLogger(), listener, bundle, testProjectID, testPoolID, newActivations())
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
