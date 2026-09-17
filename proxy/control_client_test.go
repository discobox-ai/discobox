package proxy

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy/internal/audit"
)

// controlClientFixture runs a proxy with audit recording and an authenticated
// control API, records one exchange for each of two sandboxes, and serves the
// control API. It returns the served URL and the key the proxy trusts.
func controlClientFixture(t *testing.T) (string, ed25519.PrivateKey, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	dir := t.TempDir()
	prepared, err := PrepareCertificates(PrepareOptions{
		Dir:         filepath.Join(dir, "certs"),
		ProxyURL:    "https://127.0.0.1:0",
		ServerHosts: []string{"127.0.0.1"},
		ClientIDs:   []string{"sandbox-1", "sandbox-2"},
	})
	if err != nil {
		t.Fatalf("PrepareCertificates() error = %v", err)
	}
	dbPath := filepath.Join(dir, "audit.db")
	server, err := NewServer(ctx, Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   dbPath,
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16},
		Control: ControlConfig{
			TrustPublicKey: base64.StdEncoding.EncodeToString(publicKey),
			ProjectID:      "project-1",
			WorkerID:       "pool-1",
		},
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	server.audit.RecordHTTP(audit.HTTPEvent{ClientID: "sandbox-1", Method: "GET", URL: "https://api.github.com/user", Host: "api.github.com", Status: 200, SwappedUseIDs: []string{"use_abc"}})
	server.audit.RecordHTTP(audit.HTTPEvent{ClientID: "sandbox-2", Method: "GET", URL: "https://example.com/", Host: "example.com", Status: 200})
	waitForHTTPExchange(t, dbPath, "client_id = ?", "sandbox-1")
	waitForHTTPExchange(t, dbPath, "client_id = ?", "sandbox-2")

	served := httptest.NewServer(server.ControlHandler())
	t.Cleanup(served.Close)
	return served.URL, privateKey, dbPath
}

func TestControlClientReadsWhatTheProxyAudited(t *testing.T) {
	url, key, _ := controlClientFixture(t)
	client := NewControlClient(url, key, "project-1", "pool-1", nil)
	ctx := context.Background()

	all, err := client.ListHTTP(ctx, "", AuditQuery{})
	if err != nil {
		t.Fatalf("pool-wide ListHTTP() error = %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("pool-wide read returned %d rows, want both sandboxes' 2", len(all))
	}

	// A sandbox-scoped client never sends client_id; the token carries the
	// sandbox, and the proxy narrows to it.
	scoped, err := client.ListHTTP(ctx, "sandbox-2", AuditQuery{})
	if err != nil {
		t.Fatalf("scoped ListHTTP() error = %v", err)
	}
	if len(scoped) != 1 || scoped[0].ClientID != "sandbox-2" {
		t.Fatalf("scoped read = %+v, want only sandbox-2's row", scoped)
	}

	joined, err := client.ListHTTP(ctx, "", AuditQuery{UseID: "use_abc"})
	if err != nil {
		t.Fatalf("use_id ListHTTP() error = %v", err)
	}
	if len(joined) != 1 || joined[0].ClientID != "sandbox-1" || joined[0].SwappedUseIDs != "use_abc" {
		t.Fatalf("use_id read = %+v, want the one row that spent use_abc", joined)
	}

	// Sent in a zone far from UTC: the proxy writes rows in UTC and compares
	// the bound as text, so it must be normalized on the way.
	ahead := time.Now().Add(time.Hour).In(time.FixedZone("UTC-12", -12*60*60))
	future, err := client.ListHTTP(ctx, "", AuditQuery{Since: ahead})
	if err != nil {
		t.Fatalf("since ListHTTP() error = %v", err)
	}
	if len(future) != 0 {
		t.Fatalf("since an hour ahead returned %d rows, want none", len(future))
	}
	behind := time.Now().Add(-time.Hour).In(time.FixedZone("UTC+14", 14*60*60))
	recent, err := client.ListHTTP(ctx, "", AuditQuery{Since: behind})
	if err != nil {
		t.Fatalf("since ListHTTP() error = %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("since an hour ago returned %d rows, want 2", len(recent))
	}
}

func TestControlClientWithAnUntrustedKeyIsRefused(t *testing.T) {
	url, _, _ := controlClientFixture(t)
	_, stranger, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	_, err = NewControlClient(url, stranger, "project-1", "pool-1", nil).ListHTTP(context.Background(), "", AuditQuery{})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("ListHTTP() with an untrusted key = %v, want a 401", err)
	}
}
