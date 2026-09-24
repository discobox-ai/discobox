package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy/internal/secrets"
	"github.com/elazarl/goproxy"
)

// trustFixture is a proxy with two clients in front of a TLS origin whose
// certificate no system root knows: the shape of a Kubernetes API server.
type trustFixture struct {
	server  *Server
	origin  *httptest.Server
	dsn     string
	clients map[string]*http.Client
	judged  *[]secrets.JudgeRequest
	cfg     Config
}

func newTrustFixture(t *testing.T, deny string) *trustFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("goproxy's MITM leg fails before the handler runs on Windows")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	origin := newTLSOrigin(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	t.Cleanup(origin.Close)

	dir := t.TempDir()
	prepared, err := PrepareCertificates(PrepareOptions{
		Dir:         filepath.Join(dir, "certs"),
		ProxyURL:    "https://127.0.0.1:0",
		ServerHosts: []string{"127.0.0.1", "localhost"},
		ClientIDs:   []string{"sandbox-1", "sandbox-2"},
	})
	if err != nil {
		t.Fatalf("PrepareCertificates() error = %v", err)
	}
	judged := &[]secrets.JudgeRequest{}
	cfg := Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   filepath.Join(dir, "audit.db"),
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16},
	}
	server, err := NewServer(ctx, cfg, prepared.Bundle, stubResolver{judged: judged, deny: deny})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	t.Cleanup(func() { _ = server.Close(); <-errCh })
	addr := waitForAddr(t, server)

	f := &trustFixture{server: server, origin: origin, dsn: cfg.DatabaseDSN, clients: map[string]*http.Client{}, judged: judged, cfg: cfg}
	for _, id := range []string{"sandbox-1", "sandbox-2"} {
		f.clients[id] = mitmClient(t, addr.String(), prepared.Clients[id])
	}
	return f
}

// mitmClient is a sandbox: it presents its client certificate to the proxy
// and trusts the MITM CA for everything the proxy intercepts.
func mitmClient(t *testing.T, addr string, material ClientMaterial) *http.Client {
	t.Helper()
	clientCert, err := tls.LoadX509KeyPair(material.ClientCertPath, material.ClientKeyPath)
	if err != nil {
		t.Fatalf("load client cert: %v", err)
	}
	pool := x509.NewCertPool()
	for _, p := range []string{material.MTLSCAPath, material.MITMCAPath} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read CA %s: %v", p, err)
		}
		pool.AppendCertsFromPEM(data)
	}
	proxyURL, _ := url.Parse("https://" + addr)
	return &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{clientCert},
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{"http/1.1"},
		},
	}}
}

func (f *trustFixture) endpoint(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(f.origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func (f *trustFixture) caPin() TrustPin {
	cert := f.origin.Certificate()
	return TrustPin{
		Kind:   TrustPinCA,
		SHA256: CertificateSHA256(cert),
		PEM:    string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})),
	}
}

func (f *trustFixture) apply(t *testing.T, trusts ...HostTrust) {
	t.Helper()
	cfg := f.cfg
	cfg.Trusts = trusts
	if err := f.server.ApplyConfig(cfg); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
}

// answer is what a sandbox got back, read whole.
type answer struct {
	StatusCode int
	Header     http.Header
}

func (f *trustFixture) get(t *testing.T, client string) (answer, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, f.origin.URL+"/api/v1/namespaces", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.clients[client].Do(req)
	if err != nil {
		t.Fatalf("%s GET: %v", client, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return answer{StatusCode: resp.StatusCode, Header: resp.Header}, string(body)
}

// With no pin, a refused upstream certificate is a 502 that names the host
// and the remedy, not a dropped connection, and it is audited as refused.
func TestHTTPProxyUntrustedUpstreamIsA502NamingTheHost(t *testing.T) {
	f := newTrustFixture(t, "")
	resp, body := f.get(t, "sandbox-1")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502; body %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get(UntrustedHostHeader); got != f.endpoint(t) {
		t.Fatalf("%s = %q, want %q", UntrustedHostHeader, got, f.endpoint(t))
	}
	if !strings.Contains(body, "discobox-access trust "+f.endpoint(t)) {
		t.Fatalf("body does not name the remedy: %q", body)
	}
	exchange := waitForHTTPExchange(t, f.dsn, "blocked = ? AND status = ?", true, http.StatusBadGateway)
	if !strings.HasPrefix(exchange.BlockedReason, "upstream certificate not trusted") || exchange.ClientID != "sandbox-1" {
		t.Fatalf("audit = %+v", exchange)
	}
}

// A pin is one client's. The same host, reached by a client without one, is
// refused — even while the pinned client's connection to it is open, which is
// the case a transport shared across clients would get wrong.
func TestHTTPProxyCAPinTrustsTheHostForOneClient(t *testing.T) {
	f := newTrustFixture(t, "")
	f.apply(t, HostTrust{ClientID: "sandbox-1", Host: f.endpoint(t), Pin: f.caPin(), UseIDs: []string{"use_kube"}})

	resp, body := f.get(t, "sandbox-1")
	if resp.StatusCode != http.StatusOK || body != "ok" {
		t.Fatalf("pinned client: status %d, body %q", resp.StatusCode, body)
	}
	resp, _ = f.get(t, "sandbox-2")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unpinned client: status %d, want 502", resp.StatusCode)
	}
	resp, _ = f.get(t, "sandbox-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pinned client again: status %d", resp.StatusCode)
	}
}

func TestHTTPProxyLeafSPKIPin(t *testing.T) {
	f := newTrustFixture(t, "")
	leaf := f.origin.Certificate()
	f.apply(t, HostTrust{ClientID: "sandbox-1", Host: f.endpoint(t), Pin: TrustPin{Kind: TrustPinLeafSPKI, SHA256: SPKISHA256(leaf)}})
	if resp, body := f.get(t, "sandbox-1"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %q", resp.StatusCode, body)
	}

	// A key that is not the pinned one is refused as a mismatch, and said so.
	f.apply(t, HostTrust{ClientID: "sandbox-1", Host: f.endpoint(t), Pin: TrustPin{Kind: TrustPinLeafSPKI, SHA256: strings.Repeat("0", 64)}})
	resp, body := f.get(t, "sandbox-1")
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "does not match the pin") {
		t.Fatalf("status %d, body %q", resp.StatusCode, body)
	}
}

// Every request to a trusted host is judged against the uses the host was
// trusted for, whether or not it carries a credential; one the judge refuses
// is never sent.
func TestHTTPProxyJudgesRequestsToATrustedHost(t *testing.T) {
	f := newTrustFixture(t, "")
	f.apply(t, HostTrust{ClientID: "sandbox-1", Host: f.endpoint(t), Pin: f.caPin(), UseIDs: []string{"use_kube"}})
	if resp, _ := f.get(t, "sandbox-1"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(*f.judged) != 1 {
		t.Fatalf("judged %d requests, want 1", len(*f.judged))
	}
	got := (*f.judged)[0]
	if len(got.TrustUseIDs) != 1 || got.TrustUseIDs[0] != "use_kube" || len(got.UseIDs) != 0 || got.Method != http.MethodGet {
		t.Fatalf("judge saw %+v", got)
	}

	refusing := newTrustFixture(t, "deletes are not read-only kubectl")
	refusing.apply(t, HostTrust{ClientID: "sandbox-1", Host: refusing.endpoint(t), Pin: refusing.caPin(), UseIDs: []string{"use_kube"}})
	resp, body := refusing.get(t, "sandbox-1")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "deletes are not read-only kubectl") {
		t.Fatalf("status %d, body %q", resp.StatusCode, body)
	}
}

func TestHTTPProxyTrustEndsWithItsConfigAndItsExpiry(t *testing.T) {
	f := newTrustFixture(t, "")
	f.apply(t, HostTrust{ClientID: "sandbox-1", Host: f.endpoint(t), Pin: f.caPin()})
	if resp, _ := f.get(t, "sandbox-1"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	f.apply(t)
	if resp, _ := f.get(t, "sandbox-1"); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("after removal: status %d, want 502", resp.StatusCode)
	}
	f.apply(t, HostTrust{ClientID: "sandbox-1", Host: f.endpoint(t), Pin: f.caPin(), ExpiresAt: time.Now().Add(-time.Minute)})
	if resp, _ := f.get(t, "sandbox-1"); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expired: status %d, want 502", resp.StatusCode)
	}
}

func TestProbeTLSReturnsTheChainTheHostPresents(t *testing.T) {
	f := newTrustFixture(t, "")
	probe, err := f.server.ProbeTLS(context.Background(), "sandbox-1", f.endpoint(t))
	if err != nil {
		t.Fatalf("ProbeTLS() error = %v", err)
	}
	if probe.Verified || len(probe.Chain) == 0 || CertificateSHA256(probe.Chain[0]) != CertificateSHA256(f.origin.Certificate()) {
		t.Fatalf("probe = verified %v, %d certificates", probe.Verified, len(probe.Chain))
	}

	cfg := f.cfg
	cfg.Allowlist = AllowlistConfig{Enabled: true, Domains: []string{"example.com"}}
	if err := f.server.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.ProbeTLS(context.Background(), "sandbox-1", f.endpoint(t)); !errors.Is(err, ErrProbeHostDenied) {
		t.Fatalf("ProbeTLS() on a denied host error = %v, want ErrProbeHostDenied", err)
	}
}

func TestConfigRefusesAMalformedTrust(t *testing.T) {
	good := HostTrust{ClientID: "sandbox-1", Host: "10.0.0.1:443", Pin: TrustPin{Kind: TrustPinLeafSPKI, SHA256: strings.Repeat("a", 64)}}
	base := Config{ListenAddress: "127.0.0.1:0", CertDir: t.TempDir()}
	for name, trust := range map[string]HostTrust{
		"no port":        {ClientID: good.ClientID, Host: "10.0.0.1", Pin: good.Pin},
		"no client":      {Host: good.Host, Pin: good.Pin},
		"short pin":      {ClientID: good.ClientID, Host: good.Host, Pin: TrustPin{Kind: TrustPinLeafSPKI, SHA256: "ab"}},
		"unknown kind":   {ClientID: good.ClientID, Host: good.Host, Pin: TrustPin{Kind: "off", SHA256: good.Pin.SHA256}},
		"CA without PEM": {ClientID: good.ClientID, Host: good.Host, Pin: TrustPin{Kind: TrustPinCA, SHA256: good.Pin.SHA256}},
	} {
		cfg := base
		cfg.Trusts = []HostTrust{trust}
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: Validate() accepted %+v", name, trust)
		}
	}
	cfg := base
	cfg.Trusts = []HostTrust{good}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() refused a good trust: %v", err)
	}
}

// A probe that goes out through an upstream proxy says so: behind one that
// intercepts TLS, the chain it sees is that proxy's, and the host is only
// trusted where that proxy runs. Go never proxies a loopback address, so the
// origin listens on the first non-loopback one.
func TestProbeTLSSaysWhichUpstreamItWentThrough(t *testing.T) {
	ip := nonLoopbackIPv4(t)
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Skipf("listen on %s: %v", ip, err)
	}
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	origin.Listener = listener
	origin.StartTLS()
	t.Cleanup(origin.Close)
	upstream := httptest.NewServer(goproxy.NewProxyHttpServer())
	t.Cleanup(upstream.Close)

	dir := t.TempDir()
	prepared, err := PrepareCertificates(PrepareOptions{Dir: filepath.Join(dir, "certs"), ProxyURL: "https://127.0.0.1:0", ServerHosts: []string{"127.0.0.1"}, ClientIDs: []string{"sandbox-1"}})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(context.Background(), Config{
		ListenAddress:   "127.0.0.1:0",
		CertDir:         prepared.Bundle.Dir,
		UpstreamProxy:   upstream.URL,
		UpstreamNoProxy: "example.invalid",
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	probe, err := server.ProbeTLS(context.Background(), "sandbox-1", strings.TrimPrefix(origin.URL, "https://"))
	if err != nil {
		t.Fatalf("ProbeTLS() error = %v", err)
	}
	if probe.Via != upstream.URL || len(probe.Chain) == 0 {
		t.Fatalf("probe via %q with %d certificates, want it to name %s", probe.Via, len(probe.Chain), upstream.URL)
	}
}

func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("interface addresses: %v", err)
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	t.Skip("no non-loopback IPv4 address to put an origin on")
	return ""
}

// A host that never answers is a 502 that says so, not a dropped connection,
// and it is recorded as an exchange the upstream failed rather than a refusal:
// the request may have gone out.
func TestHTTPProxyAnUpstreamThatNeverAnswersIsA502(t *testing.T) {
	f := newTrustFixture(t, "")
	// Accepts, then hangs up: what an upstream that drops the request does.
	var listenConfig net.ListenConfig
	dead, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dead.Close() })
	go func() {
		for {
			conn, err := dead.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+dead.Addr().String()+"/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.clients["sandbox-1"].Do(req)
	if err != nil {
		t.Fatalf("GET: %v, want the proxy's answer", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "did not answer") || resp.Header.Get(UpstreamProxyHeader) != "" {
		t.Fatalf("status %d, %s %q, body %q", resp.StatusCode, UpstreamProxyHeader, resp.Header.Get(UpstreamProxyHeader), body)
	}
	exchange := waitForHTTPExchange(t, f.dsn, "status = ?", http.StatusBadGateway)
	if exchange.Blocked {
		t.Fatalf("audit = %+v, want an exchange the upstream failed, not a refusal", exchange)
	}
}

// Behind an upstream proxy that hangs up — the outer proxy of a pool nested in
// a discobox, refusing a host it does not trust — the 502 names that proxy,
// which is where the remedy is.
func TestHTTPProxyNamesTheUpstreamProxyThatFailed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("goproxy's MITM leg fails before the handler runs on Windows")
	}
	ip := nonLoopbackIPv4(t)
	var listenConfig net.ListenConfig
	upstream, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	go func() {
		for {
			conn, err := upstream.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	dir := t.TempDir()
	prepared, err := PrepareCertificates(PrepareOptions{Dir: filepath.Join(dir, "certs"), ProxyURL: "https://127.0.0.1:0", ServerHosts: []string{"127.0.0.1"}, ClientIDs: []string{"sandbox-1"}})
	if err != nil {
		t.Fatal(err)
	}
	upstreamURL := "http://" + upstream.Addr().String()
	server, err := NewServer(context.Background(), Config{
		ListenAddress:   "127.0.0.1:0",
		CertDir:         prepared.Bundle.Dir,
		UpstreamProxy:   upstreamURL,
		UpstreamNoProxy: "example.invalid",
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	t.Cleanup(func() { _ = server.Close(); <-errCh })
	client := mitmClient(t, waitForAddr(t, server).String(), prepared.Clients["sandbox-1"])

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+net.JoinHostPort(ip, "6443")+"/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v, want the proxy's answer", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get(UpstreamProxyHeader) != upstreamURL || !strings.Contains(string(body), "trusted where that proxy runs") {
		t.Fatalf("status %d, %s %q, body %q", resp.StatusCode, UpstreamProxyHeader, resp.Header.Get(UpstreamProxyHeader), body)
	}
}
