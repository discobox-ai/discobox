package proxy

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// dnsAuditFixture runs a proxy with recording and a control API, records DNS
// queries through the exported RecordDNS — the way the pool's DNS server,
// running beside the proxy, does — and returns a client for the control API.
func dnsAuditFixture(t *testing.T) (*ControlClient, string, ed25519.PrivateKey) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	prepared, err := PrepareCertificates(PrepareOptions{Dir: filepath.Join(dir, "certs"), ServerHosts: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ctx, Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   filepath.Join(dir, "audit.db"),
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16},
		Control: ControlConfig{
			TrustPublicKey: base64.StdEncoding.EncodeToString(publicKey),
			ProjectID:      "project-1",
			WorkerID:       "pool-1",
		},
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	base := time.Now().UTC().Add(-time.Minute)
	server.RecordDNS(DNSAuditEvent{Time: base, ClientID: "sandbox-1", Name: "api.github.com", Type: "A", RCode: "NOERROR", Answers: []string{"192.0.2.1"}})
	server.RecordDNS(DNSAuditEvent{Time: base.Add(time.Second), ClientID: "sandbox-2", Name: "example.com", Type: "AAAA", RCode: "NXDOMAIN"})
	server.RecordDNS(DNSAuditEvent{Time: base.Add(2 * time.Second), ClientID: "sandbox-2", Name: "api.github.com", Type: "A", Error: "upstream timed out"})

	served := httptest.NewServer(server.ControlHandler())
	t.Cleanup(served.Close)
	client := NewControlClient(served.URL, privateKey, "project-1", "pool-1", nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := client.ListDNS(ctx, "", AuditDNSQueryOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 3 {
			return client, served.URL, privateKey
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy recorded %d dns rows, want 3", len(rows))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestControlClientReadsTheDNSAudit(t *testing.T) {
	client, _, _ := dnsAuditFixture(t)
	ctx := context.Background()

	scoped, err := client.ListDNS(ctx, "sandbox-2", AuditDNSQueryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 2 || scoped[0].ClientID != "sandbox-2" || scoped[1].ClientID != "sandbox-2" {
		t.Fatalf("scoped read = %+v, want only sandbox-2's queries", scoped)
	}
	if scoped[0].Error != "upstream timed out" || scoped[1].RCode != "NXDOMAIN" {
		t.Fatalf("newest first with outcomes: %+v", scoped)
	}

	named, err := client.ListDNS(ctx, "", AuditDNSQueryOptions{Name: "api.github.com", Ascending: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(named) != 2 || named[0].ClientID != "sandbox-1" || named[0].Answers != "192.0.2.1" {
		t.Fatalf("by name, oldest first = %+v", named)
	}

	one, err := client.ListDNS(ctx, "", AuditDNSQueryOptions{ID: named[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].ID != named[1].ID {
		t.Fatalf("by id %s = %+v, want that row alone", named[1].ID, one)
	}

	after, err := client.ListDNS(ctx, "", AuditDNSQueryOptions{AfterID: named[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 || after[0].ID != named[0].ID+1 {
		t.Fatalf("after %s = %+v, want the two written after it in write order", named[0].ID, after)
	}
}

// A DNS read refuses what only means something for an exchange, and a cursor
// spelled for the HTTP trail: answering 200 would read as "these matched".
func TestControlDNSRefusesHTTPFiltersAndCursors(t *testing.T) {
	client, url, key := dnsAuditFixture(t)
	token, err := CreateControlToken(key, ControlTokenClaims{ProjectID: "project-1", WorkerID: "pool-1", Scopes: []string{ScopeAuditRead}})
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"host=example.com", "use_id=use_abc", "min_status=400", "blocked=true", "after_id=http_1", "id=http_1"} {
		resp, err := client.get(context.Background(), url+"/audit/dns?"+query, token)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("GET /audit/dns?%s = %d, want 400", query, resp.StatusCode)
		}
	}
}
