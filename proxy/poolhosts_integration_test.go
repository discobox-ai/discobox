package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/wire"
)

const (
	testOriginsHost = "git.discobox.internal"
	testAssetsHost  = "assets.discobox.internal"
)

// A pool host is answered by the pool's listener for it, never the internet:
// whatever the allowlist says, a request for it is forwarded with its path and
// query, its own credential, and the sandbox the client certificate names in
// place of anything the sandbox claimed. A read-only one refuses a write at the
// proxy, before the listener sees it.
func TestHTTPProxyPoolHostsAreForwardedAsTheCertificatesSandbox(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type seen struct {
		method, uri, client, authorization, proxyAuthorization, body string
	}
	var (
		mu       sync.Mutex
		requests []seen
	)
	listener := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, seen{
			method:             r.Method,
			uri:                r.RequestURI,
			client:             r.Header.Get(PoolClientHeader),
			authorization:      r.Header.Get("Authorization"),
			proxyAuthorization: r.Header.Get("Proxy-Authorization"),
			body:               string(body),
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "origin for "+r.Header.Get(PoolClientHeader))
	})
	upstream := httptest.NewServer(listener)
	defer upstream.Close()
	// The assets listener on a Unix socket, as the pool serves it.
	dir := t.TempDir()
	socket := filepath.Join(dir, "assets.sock")
	assetsListener, err := wire.Listen(wire.UnixURL(socket))
	if err != nil {
		t.Fatal(err)
	}
	assetsServer := &http.Server{Handler: listener, ReadHeaderTimeout: time.Second}
	go func() { _ = assetsServer.Serve(assetsListener) }()
	defer assetsServer.Close()

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
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   filepath.Join(dir, "audit.db"),
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16},
		// An allowlist that admits nothing but an unrelated host: the origins
		// host is not the allowlist's to refuse.
		Allowlist: AllowlistConfig{Clients: map[string][]string{"sandbox-1": {"only.allowed.example.com"}}},
		PoolHosts: []PoolHostConfig{
			{Host: testOriginsHost, Upstream: upstream.URL + "/base/"},
			{Host: testAssetsHost, Upstream: wire.UnixURL(socket), ReadOnly: true},
		},
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	addr := waitForAddr(t, server)
	client := gateClient(t, addr.String(), prepared.Clients["sandbox-1"])

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://"+testOriginsHost+"/api/project/p/pool/q/sandboxes/sandbox-1/git-origins/primary.git/git-upload-pack?service=x",
		strings.NewReader("want"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer origin-token")
	// What a sandbox says about itself is not what the pool is told.
	req.Header.Set(PoolClientHeader, "sandbox-2")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "origin for sandbox-1" {
		t.Fatalf("origins call = %d %q, want the origin listener's answer for sandbox-1", resp.StatusCode, body)
	}
	mu.Lock()
	got := append([]seen(nil), requests...)
	mu.Unlock()
	want := seen{
		method:        http.MethodPost,
		uri:           "/base/api/project/p/pool/q/sandboxes/sandbox-1/git-origins/primary.git/git-upload-pack?service=x",
		client:        "sandbox-1",
		authorization: "Bearer origin-token",
		body:          "want",
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("origin listener saw %+v, want one request %+v", got, want)
	}

	// The assets host is forwarded the same way, to its own upstream.
	assets, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+testAssetsHost+"/blobs/sha256:00", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.Do(assets)
	if err != nil {
		t.Fatalf("assets GET error = %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "origin for sandbox-1" {
		t.Fatalf("assets GET = %d %q, want the listener's answer for sandbox-1", resp.StatusCode, body)
	}
	// And a write to it is refused by the proxy itself.
	write, err := http.NewRequestWithContext(ctx, http.MethodPut, "https://"+testAssetsHost+"/blobs/sha256:00", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.Do(write)
	if err != nil {
		t.Fatalf("assets PUT error = %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("assets PUT = %d, want 405", resp.StatusCode)
	}
	mu.Lock()
	if len(requests) != 2 || requests[1].method != http.MethodGet || requests[1].uri != "/blobs/sha256:00" || requests[1].client != "sandbox-1" {
		t.Fatalf("listener saw %+v, want the origins request and the assets GET alone", requests)
	}
	mu.Unlock()

	// Any other host is still the allowlist's.
	other, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://git.example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.Do(other)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("a host that is not the origins host was answered %d", resp.StatusCode)
		}
	}
	mu.Lock()
	if len(requests) != 2 {
		t.Fatalf("listener saw %d requests, want the pool hosts' alone", len(requests))
	}
	mu.Unlock()

	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	<-errCh
}

func TestPoolHostsRefuseAnUpstreamThatIsNoTransport(t *testing.T) {
	for _, upstream := range []string{"", "ftp://127.0.0.1/", "http://", "unix://", "127.0.0.1:17086"} {
		if _, err := newPoolHosts([]PoolHostConfig{{Host: testOriginsHost, Upstream: upstream}}); err == nil {
			t.Errorf("upstream %q accepted, want refused", upstream)
		}
	}
	if _, err := newPoolHosts([]PoolHostConfig{{Upstream: "http://127.0.0.1:1"}}); err == nil {
		t.Error("a pool host naming no host was accepted")
	}
	twice := PoolHostConfig{Host: testOriginsHost, Upstream: "http://127.0.0.1:1"}
	if _, err := newPoolHosts([]PoolHostConfig{twice, twice}); err == nil {
		t.Error("a host named twice was accepted")
	}
	hosts, err := newPoolHosts(nil)
	if err != nil || hosts != nil {
		t.Fatalf("no pool hosts = %v, %v; want none", hosts, err)
	}
	if hosts.answering(testOriginsHost) != nil {
		t.Fatal("no pool hosts answered a host")
	}
}
