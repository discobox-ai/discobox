package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/wire"
	"github.com/discobox-ai/discobox/proxy/internal/audit"
)

const (
	testOriginsHost  = "git.discobox.internal"
	testAssetsHost   = "assets.discobox.internal"
	testRegistryHost = "registry.discobox.internal"
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
	listener := ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
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
	// The assets listener on a Unix socket, as the pool serves it. The
	// socket's directory is short, since a socket path over ~100 bytes does
	// not bind, and a test's own temporary directory can be longer.
	dir := t.TempDir()
	sockets, err := os.MkdirTemp("", "ph")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockets) })
	socket := filepath.Join(sockets, "assets.sock")
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

// A pool host's response is never cached, even on a path the cache would
// admit — the registry's blobs are already on the pool's disk — and a host
// whose bodies are unspooled, the build registry, spools neither its request
// bodies nor its responses' while still auditing every exchange (ADR
// 26-10-29-160 §4). A pool host that spools still spools its responses.
func TestPoolHostsAreNotCachedAndUnspooledBodiesAreNotRecorded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const blob = "/v2/sbx_1/discobox-build/blobs/sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	var (
		mu   sync.Mutex
		hits = map[string]int{}
	)
	listener := ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		hits[r.Method+" "+r.URL.Path]++
		mu.Unlock()
		_, _ = io.WriteString(w, "layer bytes")
	})
	upstream := httptest.NewServer(listener)
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
	dbPath := filepath.Join(dir, "audit.db")
	bodyDir := filepath.Join(dir, "bodies")
	server, err := NewServer(ctx, Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   dbPath,
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16, BodyDir: bodyDir},
		// The pool's own cache patterns, which a registry blob path matches.
		Cache: CacheConfig{
			Enabled:      true,
			Dir:          filepath.Join(dir, "cache"),
			MaxSizeBytes: 1024 * 1024,
			Patterns:     []string{`^/v2/.*/blobs/sha256:[a-fA-F0-9]{64}$`},
		},
		PoolHosts: []PoolHostConfig{
			{Host: testRegistryHost, Upstream: upstream.URL, UnspooledBodies: true},
			{Host: testAssetsHost, Upstream: upstream.URL, ReadOnly: true},
		},
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	t.Cleanup(closeProxyServer(t, server, errCh))
	addr := waitForAddr(t, server)
	client := gateClient(t, addr.String(), prepared.Clients["sandbox-1"])

	do := func(method, host string, body string) {
		t.Helper()
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, "https://"+host+blob, reader)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, host, err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(got) != "layer bytes" {
			t.Fatalf("%s %s = %d %q, want the listener's answer", method, host, resp.StatusCode, got)
		}
	}
	for range 2 {
		do(http.MethodGet, testRegistryHost, "")
		do(http.MethodGet, testAssetsHost, "")
	}
	do(http.MethodPut, testRegistryHost, "pushed layer")

	mu.Lock()
	if hits["GET "+blob] != 4 || hits["PUT "+blob] != 1 {
		t.Fatalf("listener hits = %v, want every GET to reach it: nothing cached", hits)
	}
	mu.Unlock()

	put := waitForHTTPExchange(t, dbPath, "client_id = ? AND method = ? AND host = ?", "sandbox-1", http.MethodPut, testRegistryHost)
	if put.RequestBodyFile != "" || put.RequestBodyFormat != "" || put.ResponseBodyFile != "" || put.ResponseBodyFormat != "" {
		t.Fatalf("registry PUT row spooled bodies: request %q/%q response %q/%q", put.RequestBodyFormat, put.RequestBodyFile, put.ResponseBodyFormat, put.ResponseBodyFile)
	}
	get := waitForHTTPExchange(t, dbPath, "client_id = ? AND method = ? AND host = ?", "sandbox-1", http.MethodGet, testRegistryHost)
	if get.CacheStored || get.CacheHit || get.ResponseBodyFile != "" || get.ResponseBodyFormat != "" {
		t.Fatalf("registry GET row = stored %v hit %v body %q/%q, want neither cached nor spooled", get.CacheStored, get.CacheHit, get.ResponseBodyFormat, get.ResponseBodyFile)
	}
	assets := waitForHTTPExchange(t, dbPath, "client_id = ? AND method = ? AND host = ? AND response_body_file <> ''", "sandbox-1", http.MethodGet, testAssetsHost)
	if assets.CacheStored || assets.CacheHit || assets.ResponseBodyFormat != audit.BodyFormatRaw {
		t.Fatalf("assets GET row = stored %v hit %v format %q, want spooled raw and not cached", assets.CacheStored, assets.CacheHit, assets.ResponseBodyFormat)
	}
	assertSpoolFile(t, filepath.Join(bodyDir, filepath.FromSlash(assets.ResponseBodyFile)), "layer bytes")
}
