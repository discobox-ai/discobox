package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/gormdb"
	"github.com/discobox-ai/discobox/proxy/internal/audit"
)

func TestSOCKSProxyMTLSIdentityDeniedAudit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
		Recording: RecordingConfig{
			Enabled:   true,
			QueueSize: 16,
		},
		Allowlist: AllowlistConfig{
			Enabled: true,
			Domains: []string{"example.com"},
		},
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()
	var closeOnce sync.Once
	closeServer := func() {
		closeOnce.Do(func() {
			if err := server.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			select {
			case err := <-errCh:
				if err != nil {
					t.Fatalf("ListenAndServe() error = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for proxy shutdown")
			}
		})
	}
	t.Cleanup(closeServer)
	addr := waitForAddr(t, server)

	clientMaterial := prepared.Clients["sandbox-1"]
	clientCert, err := tls.LoadX509KeyPair(clientMaterial.ClientCertPath, clientMaterial.ClientKeyPath)
	if err != nil {
		t.Fatalf("load client certificate: %v", err)
	}
	caBytes, err := os.ReadFile(clientMaterial.MTLSCAPath)
	if err != nil {
		t.Fatalf("read mTLS CA: %v", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caBytes) {
		t.Fatal("failed to parse mTLS CA")
	}

	dialer := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config: &tls.Config{
			RootCAs:      caPool,
			Certificates: []tls.Certificate{clientCert},
			MinVersion:   tls.VersionTLS12,
		},
	}
	conn, err := dialer.DialContext(ctx, "tcp", addr.String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("write socks greeting: %v", err)
	}
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		t.Fatalf("read socks greeting: %v", err)
	}
	if greeting[0] != 0x05 || greeting[1] != 0x00 {
		t.Fatalf("unexpected greeting response: %#v", greeting)
	}

	request := []byte{0x05, 0x01, 0x00, 0x01, 127, 0, 0, 1, 0, 1}
	if _, err := conn.Write(request); err != nil {
		t.Fatalf("write socks connect: %v", err)
	}
	response := make([]byte, 10)
	if _, err := io.ReadFull(conn, response); err != nil {
		t.Fatalf("read socks response: %v", err)
	}
	if response[1] != 0x02 {
		t.Fatalf("SOCKS reply = %#x, want rule failure", response[1])
	}
	_ = conn.Close()

	closeServer()

	pools, err := gormdb.Open(gormdb.Config{DSN: dbPath})
	if err != nil {
		t.Fatalf("open audit db: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	var connect audit.SOCKSConnect
	if err := pools.Read.Where("client_id = ?", "sandbox-1").First(&connect).Error; err != nil {
		t.Fatalf("read socks audit: %v", err)
	}
	if connect.Allowed {
		t.Fatal("expected denied SOCKS connect")
	}
	if connect.Destination != "127.0.0.1" {
		t.Fatalf("Destination = %q", connect.Destination)
	}
}
