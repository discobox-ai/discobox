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
)

const testOriginsHost = "git.discobox.internal"

// The origins host is answered by the pool's origin listener, never the
// internet: whatever the allowlist says, a request for it is forwarded with its
// path and query, its own credential, and the sandbox the client certificate
// names in place of anything the sandbox claimed.
func TestHTTPProxyOriginsHostIsForwardedAsTheCertificatesSandbox(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type seen struct {
		method, uri, client, authorization, proxyAuthorization, body string
	}
	var (
		mu       sync.Mutex
		requests []seen
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, seen{
			method:             r.Method,
			uri:                r.RequestURI,
			client:             r.Header.Get(OriginClientHeader),
			authorization:      r.Header.Get("Authorization"),
			proxyAuthorization: r.Header.Get("Proxy-Authorization"),
			body:               string(body),
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "origin for "+r.Header.Get(OriginClientHeader))
	}))
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
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   filepath.Join(dir, "audit.db"),
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16},
		// An allowlist that admits nothing but an unrelated host: the origins
		// host is not the allowlist's to refuse.
		Allowlist: AllowlistConfig{Enabled: true, Domains: []string{"only.allowed.example.com"}},
		Origins:   OriginsConfig{Host: testOriginsHost, Upstream: upstream.URL + "/base/"},
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
	req.Header.Set(OriginClientHeader, "sandbox-2")
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
	if len(requests) != 1 {
		t.Fatalf("origin listener saw %d requests, want the origins host's alone", len(requests))
	}
	mu.Unlock()

	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	<-errCh
}

func TestOriginsConfigRefusesAnUpstreamThatIsNotHTTP(t *testing.T) {
	for _, upstream := range []string{"", "unix:///run/origins.sock", "127.0.0.1:17086"} {
		if _, err := newOriginForwarder(OriginsConfig{Host: testOriginsHost, Upstream: upstream}); err == nil {
			t.Errorf("upstream %q accepted, want refused", upstream)
		}
	}
	forwarder, err := newOriginForwarder(OriginsConfig{})
	if err != nil || forwarder != nil {
		t.Fatalf("no origins host = %v, %v; want no forwarder", forwarder, err)
	}
	if forwarder.answers(testOriginsHost) {
		t.Fatal("a nil forwarder answered a host")
	}
}
