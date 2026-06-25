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

	"github.com/obot-platform/discobox/localipc"
)

func TestListenAllFailsWhenAnyEndpointCannotBind(t *testing.T) {
	ctx := context.Background()
	var listenConfig net.ListenConfig
	occupied, err := listenConfig.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy tcp port: %v", err)
	}
	defer occupied.Close()

	socketPath := filepath.Join(t.TempDir(), "server.sock")
	listeners, err := listenAll([]string{
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
	endpoint := "unix://" + filepath.Join(t.TempDir(), "server.sock")
	listener, _, cleanup, err := localipc.Listen(endpoint)
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

	if err := shutdownExistingLocalServer(context.Background(), []string{endpoint, "http://127.0.0.1:1"}, 0); err != nil {
		t.Fatalf("shutdownExistingLocalServer() error = %v", err)
	}
	if shutdowns.Load() != 1 {
		t.Fatalf("shutdown requests = %d, want 1", shutdowns.Load())
	}
}

func TestShutdownExistingLocalServerIgnoresUnavailableSocket(t *testing.T) {
	endpoint := "unix://" + filepath.Join(t.TempDir(), "missing.sock")
	if err := shutdownExistingLocalServer(context.Background(), []string{endpoint}, 0); err != nil {
		t.Fatalf("shutdownExistingLocalServer() error = %v", err)
	}
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

	if err := shutdownExistingLocalServer(ctx, []string{
		"unix://" + filepath.Join(t.TempDir(), "missing.sock"),
		"http://" + listener.Addr().String(),
	}, 0); err != nil {
		t.Fatalf("shutdownExistingLocalServer() error = %v", err)
	}
	if shutdowns.Load() != 1 {
		t.Fatalf("shutdown requests = %d, want 1", shutdowns.Load())
	}
}
