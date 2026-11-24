package proxy

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newLifecycleServer(t *testing.T) *Server {
	t.Helper()
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
	server, err := NewServer(context.Background(), Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   filepath.Join(dir, "audit.db"),
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	return server
}

// A Close that lands before ListenAndServe has opened its listener still stops
// it: ListenAndServe must not go on to listen and serve a closed server.
func TestListenAndServeAfterCloseDoesNotServe(t *testing.T) {
	server := newLifecycleServer(t)
	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		addr := server.Addr()
		t.Fatalf("ListenAndServe on a closed server is still serving on %v", addr)
	}
}

// Addr is polled while ListenAndServe starts; under -race the two must not
// race over the listener.
func TestAddrWhileListenAndServeStarts(t *testing.T) {
	server := newLifecycleServer(t)
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	deadline := time.Now().Add(5 * time.Second)
	for server.Addr() == nil {
		if time.Now().After(deadline) {
			t.Fatal("Addr() never reported the listener")
		}
		time.Sleep(time.Millisecond)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("ListenAndServe() error = %v", err)
	}
}
