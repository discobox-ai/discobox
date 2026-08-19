//go:build !windows

package endpoint

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoopbackProxyForwardsToUnixSocket(t *testing.T) {
	endpoint := "unix://" + filepath.Join(t.TempDir(), "server.sock")
	listener, _, cleanup, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer cleanup()

	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read body: %v", err)
			}
			_, _ = io.WriteString(w, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(body))
		}),
	}
	defer server.Close()
	go func() {
		_ = server.Serve(listener)
	}()

	proxy, err := StartLoopbackProxy(context.Background(), endpoint)
	if err != nil {
		t.Fatalf("StartLoopbackProxy() error = %v", err)
	}
	defer proxy.Close()
	if !strings.HasPrefix(proxy.BaseURL(), "http://127.0.0.1:") {
		t.Fatalf("BaseURL() = %q, want a loopback http URL", proxy.BaseURL())
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		proxy.BaseURL()+"/projects/p/sandboxes/s/git-repositories/primary.git/git-upload-pack",
		strings.NewReader("want"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	want := "POST /projects/p/sandboxes/s/git-repositories/primary.git/git-upload-pack Bearer token want"
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}

func TestLoopbackProxyCloseReleasesAddress(t *testing.T) {
	endpoint := "unix://" + filepath.Join(t.TempDir(), "server.sock")
	proxy, err := StartLoopbackProxy(context.Background(), endpoint)
	if err != nil {
		t.Fatalf("StartLoopbackProxy() error = %v", err)
	}
	baseURL := proxy.BaseURL()
	if err := proxy.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("request succeeded after Close(), want a connection error")
	}
}
