//go:build !windows

package endpoint

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestHTTPClientUsesUnixSocket(t *testing.T) {
	endpoint := "unix://" + filepath.Join(t.TempDir(), "server.sock")
	listener, _, cleanup, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer cleanup()

	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/healthz" {
				t.Fatalf("path = %q, want /healthz", r.URL.Path)
			}
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	defer server.Close()
	go func() {
		_ = server.Serve(listener)
	}()

	baseURL, client, err := HTTPClient(endpoint, nil)
	if err != nil {
		t.Fatalf("HTTPClient() error = %v", err)
	}
	if baseURL != LogicalHTTPBaseURL {
		t.Fatalf("baseURL = %q, want %q", baseURL, LogicalHTTPBaseURL)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

func TestEnsureRunningUsesExistingUnixServer(t *testing.T) {
	endpoint := "unix://" + filepath.Join(t.TempDir(), "server.sock")
	listener, _, cleanup, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer cleanup()

	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/healthz" {
				t.Fatalf("path = %q, want /healthz", r.URL.Path)
			}
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	defer server.Close()
	go func() {
		_ = server.Serve(listener)
	}()

	if err := EnsureRunning(context.Background(), LaunchOptions{
		Endpoint:     endpoint,
		Command:      "",
		ProbeTimeout: time.Second,
	}); err != nil {
		t.Fatalf("EnsureRunning() error = %v", err)
	}
}
