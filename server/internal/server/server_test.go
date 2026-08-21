//go:build !windows

package server

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/endpoint"
)

func TestListenAllFailsWhenAnyEndpointCannotBind(t *testing.T) {
	ctx := context.Background()
	var listenConfig net.ListenConfig
	occupied, err := listenConfig.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy tcp port: %v", err)
	}
	defer occupied.Close()

	socketPath := testSocketPath(t)
	// Bound the reclaim so the test doesn't wait the full reclaimTimeout while
	// the occupied (non-HTTP) port refuses to release.
	reclaimCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	listeners, err := listenAll(reclaimCtx, []string{
		"unix://" + socketPath,
		"http://" + occupied.Addr().String(),
	})
	if err == nil {
		cleanupListeners(listeners)
		t.Fatalf("listenAll() error = nil, want bind error")
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("unix socket %q still accepts connections after failed listenAll", socketPath)
	}
}

func TestShutdownExistingLocalServerUsesLocalEndpoint(t *testing.T) {
	raw := "unix://" + testSocketPath(t)
	listener, _, cleanup, err := endpoint.Listen(raw)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer cleanup()

	var shutdowns atomic.Int64
	httpServer := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/shutdown" {
				t.Fatalf("request = %s %s, want POST /shutdown", r.Method, r.URL.Path)
			}
			shutdowns.Add(1)
			w.WriteHeader(http.StatusAccepted)
		}),
	}
	defer httpServer.Close()
	go func() {
		_ = httpServer.Serve(listener)
	}()

	shutdownExistingLocalServer(context.Background(), []string{raw, "http://127.0.0.1:1"})
	if shutdowns.Load() != 1 {
		t.Fatalf("shutdown requests = %d, want 1", shutdowns.Load())
	}
}

func TestShutdownExistingLocalServerIgnoresUnavailableSocket(t *testing.T) {
	raw := "unix://" + filepath.Join(t.TempDir(), "missing.sock")
	shutdownExistingLocalServer(context.Background(), []string{raw})
}

func TestShutdownExistingLocalServerFallsBackToHTTP(t *testing.T) {
	ctx := context.Background()
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}

	var shutdowns atomic.Int64
	httpServer := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/shutdown" {
				t.Fatalf("request = %s %s, want POST /shutdown", r.Method, r.URL.Path)
			}
			shutdowns.Add(1)
			w.WriteHeader(http.StatusAccepted)
		}),
	}
	defer httpServer.Close()
	go func() {
		_ = httpServer.Serve(listener)
	}()

	shutdownExistingLocalServer(ctx, []string{
		"unix://" + filepath.Join(t.TempDir(), "missing.sock"),
		"http://" + listener.Addr().String(),
	})
	if shutdowns.Load() != 1 {
		t.Fatalf("shutdown requests = %d, want 1", shutdowns.Load())
	}
}
