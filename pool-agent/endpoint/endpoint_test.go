package endpoint

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSchemes(t *testing.T) {
	tests := []struct {
		raw     string
		scheme  string
		host    string
		port    uint32
		path    string
		wantErr bool
	}{
		{raw: "http://host.docker.internal:8080", scheme: "http", host: "host.docker.internal:8080"},
		{raw: "https://cp.example.com", scheme: "https", host: "cp.example.com"},
		{raw: "vsock://2:3001", scheme: "vsock", host: "2", port: 3001},
		{raw: "vsock://:3002", scheme: "vsock", port: 3002},
		{raw: "unix:///run/discobox/cp.sock", scheme: "unix", path: "/run/discobox/cp.sock"},

		{raw: "", wantErr: true},
		{raw: "ftp://nope", wantErr: true},
		{raw: "http://", wantErr: true},
		{raw: "vsock://2", wantErr: true},          // no port
		{raw: "vsock://2:80", wantErr: true},       // reserved port
		{raw: "vsock://abc:3001", wantErr: true},   // non-numeric CID
		{raw: "vsock://2:notaport", wantErr: true}, // non-numeric port
		{raw: "unix://", wantErr: true},            // no path
	}
	for _, tc := range tests {
		got, err := Parse(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("Parse(%q) should have failed, got %#v", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.raw, err)
			continue
		}
		if got.Scheme != tc.scheme || got.Host != tc.host || got.Port != tc.port || got.Path != tc.path {
			t.Errorf("Parse(%q) = %+v, want scheme=%q host=%q port=%d path=%q",
				tc.raw, got, tc.scheme, tc.host, tc.port, tc.path)
		}
	}
}

// An IP endpoint keeps its own authority; every other transport reaches a peer
// the dialer already fixed, so it uses the stable logical authority.
func TestBaseURL(t *testing.T) {
	for raw, want := range map[string]string{
		"http://host.docker.internal:8080/": "http://host.docker.internal:8080",
		"https://cp.example.com":            "https://cp.example.com",
		"vsock://2:3001":                    LogicalHTTPBaseURL,
		"unix:///run/discobox/cp.sock":      LogicalHTTPBaseURL,
	} {
		parsed, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if got := parsed.BaseURL(); got != want {
			t.Errorf("Parse(%q).BaseURL() = %q, want %q", raw, got, want)
		}
	}
}

// The whole point of the package: a caller gets a working client from a URL
// without knowing the transport. Exercised over a real Unix socket, which is
// the scheme the wslc guest helper will serve.
func TestHTTPClientOverUnixTransport(t *testing.T) {
	dir, err := os.MkdirTemp("", "ep")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := unixURL(filepath.Join(dir, "cp.sock"))

	listener, err := Listen(socket)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	server := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok:" + r.URL.Path)) }),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	baseURL, client, err := HTTPClient(socket, 10*time.Second)
	if err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
	if baseURL != LogicalHTTPBaseURL {
		t.Fatalf("baseURL = %q, want %q", baseURL, LogicalHTTPBaseURL)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request over unix endpoint: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got, want := string(body), "ok:/healthz"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// An http endpoint must dial normally and keep its own authority, so existing
// local-docker pools behave exactly as before.
func TestHTTPClientOverTCPTransport(t *testing.T) {
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	server := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("tcp-ok")) }),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	raw := "http://" + listener.Addr().String()
	baseURL, client, err := HTTPClient(raw, 10*time.Second)
	if err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
	if baseURL != raw {
		t.Fatalf("baseURL = %q, want %q", baseURL, raw)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+"/", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "tcp-ok" {
		t.Fatalf("body = %q, want tcp-ok", body)
	}
}

// The URL builders must round-trip through Parse, since the engine renders URLs
// with them and the agent parses them back.
func TestURLBuildersRoundTrip(t *testing.T) {
	if got, err := Parse(VSOCKURL(2, 3001)); err != nil || got.Scheme != "vsock" || got.Host != "2" || got.Port != 3001 {
		t.Fatalf("VSOCKURL round trip = %+v, err=%v", got, err)
	}
	if got, err := Parse(VSOCKListenURL(3002)); err != nil || got.Scheme != "vsock" || got.Host != "" || got.Port != 3002 {
		t.Fatalf("VSOCKListenURL round trip = %+v, err=%v", got, err)
	}
	got, err := Parse(TCPListenURL(3002))
	if err != nil || got.Scheme != "http" || !strings.HasSuffix(got.Host, ":3002") {
		t.Fatalf("TCPListenURL round trip = %+v, err=%v", got, err)
	}
}

// A vsock endpoint must produce a dialer on every platform even though dialing
// only succeeds on Linux guests, so the server can render and validate these
// URLs while running on Windows or macOS.
func TestVSOCKDialerResolvesOnAllPlatforms(t *testing.T) {
	parsed, err := Parse("vsock://2:3001")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	dial, err := parsed.DialContext()
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	if dial == nil {
		t.Fatal("DialContext returned no dialer")
	}
}

// unixURL renders a socket path as a unix endpoint URL. A POSIX path uses the
// authority-less form; a Windows drive path must use the opaque form, since
// "unix://C:/..." would parse the drive letter as the host.
func unixURL(path string) string {
	slashed := filepath.ToSlash(path)
	if strings.HasPrefix(slashed, "/") {
		return "unix://" + slashed
	}
	return "unix:" + slashed
}
