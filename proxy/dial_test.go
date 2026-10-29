package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOwnAddressIsLoopbackOrUnspecifiedInEitherFamily(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1":        true,
		"127.1.2.3":        true,
		"::1":              true,
		"0.0.0.0":          true,
		"::":               true,
		"::ffff:127.0.0.1": true,
		"::ffff:0.0.0.0":   true,
		"10.0.0.1":         false,
		"172.17.0.1":       false,
		"93.184.216.34":    false,
		"2001:db8::1":      false,
	} {
		if got := ownAddress(netip.MustParseAddr(addr)); got != want {
			t.Errorf("ownAddress(%s) = %v, want %v", addr, got, want)
		}
	}
}

// The refusal is decided on the address the dialer connects to, after any name
// is resolved, so a name for loopback is refused like the literal, and the
// upstream proxy's own hop is not a destination at all.
func TestDestinationDialerRefusesTheHostsOwnAddresses(t *testing.T) {
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	ctx := context.Background()

	refusing := &destinationDialer{}
	for _, addr := range []string{listener.Addr().String(), net.JoinHostPort("localhost", port)} {
		conn, err := refusing.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("dial %s was allowed, want the pool's own loopback refused", addr)
		}
		if !errors.Is(err, errOwnLoopback) {
			t.Fatalf("dial %s error = %v, want errOwnLoopback", addr, err)
		}
	}

	via := &destinationDialer{via: listener.Addr().String()}
	conn, err := via.DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial the upstream proxy's hop on loopback: %v", err)
	}
	_ = conn.Close()

	allowing := &destinationDialer{allowLoopback: true}
	conn, err = allowing.DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial with loopback allowed: %v", err)
	}
	_ = conn.Close()
}

func TestProxyDialAddressAddsTheSchemesDefaultPort(t *testing.T) {
	for raw, want := range map[string]string{
		"http://127.0.0.1:17008": "127.0.0.1:17008",
		"http://proxy.internal":  "proxy.internal:80",
		"https://proxy.internal": "proxy.internal:443",
		"socks5://[::1]":         "[::1]:1080",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := proxyDialAddress(u); got != want {
			t.Errorf("proxyDialAddress(%s) = %q, want %q", raw, got, want)
		}
	}
	if got := proxyDialAddress(nil); got != "" {
		t.Errorf("proxyDialAddress(nil) = %q, want empty", got)
	}
}

// A pool proxy runs in the pool's network namespace, beside services that
// listen on its loopback for the pool alone. A sandbox's request for one —
// plain HTTP, through a MITM'd CONNECT, by a name that resolves there, or over
// SOCKS — is refused, and audited as blocked, before anything is sent (ADR
// 26-10-29-160 §3). An upstream proxy on loopback is still this proxy's hop.
func TestPoolProxyRefusesItsOwnLoopback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var originHits atomic.Int32
	origin := newOrigin(func(w http.ResponseWriter, _ *http.Request) {
		originHits.Add(1)
		_, _ = io.WriteString(w, "loopback service")
	})
	defer origin.Close()
	tlsOrigin := newTLSOrigin(func(w http.ResponseWriter, _ *http.Request) {
		originHits.Add(1)
		_, _ = io.WriteString(w, "loopback service")
	})
	defer tlsOrigin.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(origin.URL, "http://"))

	dir := t.TempDir()
	prepared, err := PrepareCertificates(PrepareOptions{
		Dir:         filepath.Join(dir, "certs"),
		ProxyURL:    "https://127.0.0.1:0",
		ServerHosts: []string{"127.0.0.1", "localhost"},
		ClientIDs:   []string{"sandbox-1"},
	})
	if err != nil {
		t.Fatalf("PrepareCertificates() error = %v", err)
	}
	dbPath := filepath.Join(dir, "audit.db")
	server, err := NewServer(ctx, Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   dbPath,
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16},
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	t.Cleanup(closeProxyServer(t, server, errCh))
	addr := waitForAddr(t, server)
	client := gateClient(t, addr.String(), prepared.Clients["sandbox-1"])
	// The MITM'd leg of an https request trusts the MITM CA (gateClient).
	for _, target := range []string{origin.URL + "/v2/_catalog", "http://localhost:" + port + "/v2/_catalog", tlsOrigin.URL + "/v2/_catalog"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "pool's own loopback") {
			t.Fatalf("GET %s = %d %q, want a 403 naming the pool's own loopback", target, resp.StatusCode, body)
		}
	}
	if originHits.Load() != 0 {
		t.Fatalf("the loopback service was reached %d times", originHits.Load())
	}
	exchange := waitForHTTPExchange(t, dbPath, "client_id = ? AND blocked = ? AND status = ?", "sandbox-1", true, http.StatusForbidden)
	if !strings.Contains(exchange.BlockedReason, "pool's own loopback") {
		t.Fatalf("blocked reason = %q, want it to name the pool's own loopback", exchange.BlockedReason)
	}

	// SOCKS dials through the same refusal.
	conn := socksConnect(ctx, t, addr.String(), prepared.Clients["sandbox-1"], net.ParseIP("127.0.0.1").To4(), port)
	defer conn.Close()
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read socks reply: %v", err)
	}
	if reply[1] == 0x00 {
		t.Fatal("SOCKS connect to the pool's loopback succeeded")
	}
	if originHits.Load() != 0 {
		t.Fatalf("the loopback service was reached %d times", originHits.Load())
	}
}

// An upstream proxy is this proxy's own hop: in a pool nested in a sandbox it
// is that sandbox's forwarder, on loopback, and it is still dialed.
func TestPoolProxyStillReachesAnUpstreamProxyOnLoopback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var forwarded string
	upstream := newOrigin(func(w http.ResponseWriter, r *http.Request) {
		forwarded = r.URL.String()
		_, _ = io.WriteString(w, "via upstream")
	})
	defer upstream.Close()

	dir := t.TempDir()
	prepared, err := PrepareCertificates(PrepareOptions{
		Dir:         filepath.Join(dir, "certs"),
		ProxyURL:    "https://127.0.0.1:0",
		ServerHosts: []string{"127.0.0.1", "localhost"},
		ClientIDs:   []string{"sandbox-1"},
	})
	if err != nil {
		t.Fatalf("PrepareCertificates() error = %v", err)
	}
	server, err := NewServer(ctx, Config{
		ListenAddress:   "127.0.0.1:0",
		CertDir:         prepared.Bundle.Dir,
		DatabaseDSN:     filepath.Join(dir, "audit.db"),
		Recording:       RecordingConfig{Enabled: true, QueueSize: 16},
		UpstreamProxy:   upstream.URL,
		UpstreamNoProxy: "exempt.invalid",
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	t.Cleanup(closeProxyServer(t, server, errCh))
	addr := waitForAddr(t, server)

	client := mtlsHTTPClient(t, addr.String(), prepared.Clients["sandbox-1"])
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "via upstream" || forwarded != "http://example.com/x" {
		t.Fatalf("request through the upstream = %d %q (forwarded %q), want the upstream's answer", resp.StatusCode, body, forwarded)
	}
}

// socksConnect opens an mTLS connection to the proxy, greets it as SOCKS5 and
// asks it to connect to ip:port, leaving the reply unread.
func socksConnect(ctx context.Context, t *testing.T, addr string, material ClientMaterial, ip net.IP, port string) net.Conn {
	t.Helper()
	clientCert, err := tls.LoadX509KeyPair(material.ClientCertPath, material.ClientKeyPath)
	if err != nil {
		t.Fatalf("load client certificate: %v", err)
	}
	caBytes, err := os.ReadFile(material.MTLSCAPath)
	if err != nil {
		t.Fatalf("read mTLS CA: %v", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caBytes) {
		t.Fatal("failed to parse mTLS CA")
	}
	dialer := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config:    &tls.Config{RootCAs: caPool, Certificates: []tls.Certificate{clientCert}, MinVersion: tls.VersionTLS12},
	}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("write socks greeting: %v", err)
	}
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		t.Fatalf("read socks greeting: %v", err)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	request := append([]byte{0x05, 0x01, 0x00, 0x01}, ip...)
	request = append(request, byte(portNumber>>8), byte(portNumber))
	if _, err := conn.Write(request); err != nil {
		t.Fatalf("write socks connect: %v", err)
	}
	return conn
}
