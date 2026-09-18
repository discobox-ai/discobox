package proxy

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net/http"
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

	base := time.Now().UTC().Add(-time.Minute)
	server.audit.RecordHTTP(audit.HTTPEvent{Time: base, ClientID: "sandbox-1", Method: "GET", URL: "https://api.github.com/user", Host: "api.github.com", Status: 200, SwappedUseIDs: []string{"use_abc"}})
	server.audit.RecordHTTP(audit.HTTPEvent{Time: base.Add(time.Second), ClientID: "sandbox-2", Method: "GET", URL: "https://example.com/", Host: "example.com", Status: 200})
	server.audit.RecordHTTP(audit.HTTPEvent{Time: base.Add(2 * time.Second), ClientID: "sandbox-2", Method: "GET", URL: "https://evil.example/", Host: "evil.example", Status: 403, Blocked: true, BlockedReason: "host denied"})
	waitForHTTPExchange(t, dbPath, "client_id = ?", "sandbox-1")
	waitForHTTPExchange(t, dbPath, "host = ?", "example.com")
	waitForHTTPExchange(t, dbPath, "host = ?", "evil.example")

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
	if len(all) != 3 {
		t.Fatalf("pool-wide read returned %d rows, want all 3", len(all))
	}

	// A sandbox-scoped client never sends client_id; the token carries the
	// sandbox, and the proxy narrows to it.
	scoped, err := client.ListHTTP(ctx, "sandbox-2", AuditQuery{})
	if err != nil {
		t.Fatalf("scoped ListHTTP() error = %v", err)
	}
	if len(scoped) != 2 || scoped[0].ClientID != "sandbox-2" || scoped[1].ClientID != "sandbox-2" {
		t.Fatalf("scoped read = %+v, want only sandbox-2's rows", scoped)
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
	if len(recent) != 3 {
		t.Fatalf("since an hour ago returned %d rows, want 3", len(recent))
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

func TestControlClientOrdersAndFiltersByStatus(t *testing.T) {
	url, key, _ := controlClientFixture(t)
	client := NewControlClient(url, key, "project-1", "pool-1", nil)
	ctx := context.Background()

	hosts := func(rows []AuditHTTPExchange) []string {
		out := make([]string, 0, len(rows))
		for _, row := range rows {
			out = append(out, row.Host)
		}
		return out
	}
	newest, err := client.ListHTTP(ctx, "", AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if got := hosts(newest); len(got) != 3 || got[0] != "evil.example" || got[2] != "api.github.com" {
		t.Fatalf("default order = %v, want newest first", got)
	}
	// A follower reads forward: oldest first, so a limit cuts off the newest
	// rather than silently skipping rows between two reads.
	oldest, err := client.ListHTTP(ctx, "", AuditQuery{Ascending: true, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := hosts(oldest); len(got) != 2 || got[0] != "api.github.com" || got[1] != "example.com" {
		t.Fatalf("ascending with a limit = %v, want the two oldest in order", got)
	}

	blocked := true
	refused, err := client.ListHTTP(ctx, "", AuditQuery{Blocked: &blocked})
	if err != nil {
		t.Fatal(err)
	}
	if got := hosts(refused); len(got) != 1 || got[0] != "evil.example" {
		t.Fatalf("blocked = %v, want only the refused request", got)
	}
	clientErrors, err := client.ListHTTP(ctx, "", AuditQuery{MinStatus: 400, MaxStatus: 499})
	if err != nil {
		t.Fatal(err)
	}
	if got := hosts(clientErrors); len(got) != 1 || got[0] != "evil.example" {
		t.Fatalf("status 400-499 = %v, want only the 403", got)
	}
}

// A SOCKS connect has no HTTP status or swapped credential. A filter on either
// is refused, not answered with every row.
func TestControlSOCKSRefusesHTTPOnlyFilters(t *testing.T) {
	url, key, _ := controlClientFixture(t)
	token, err := CreateControlToken(key, ControlTokenClaims{ProjectID: "project-1", WorkerID: "pool-1", Scopes: []string{ScopeAuditRead}})
	if err != nil {
		t.Fatal(err)
	}
	for _, param := range httpOnlyControlParams {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url+"/audit/socks?"+param+"=1", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("/audit/socks?%s: status %d, want 400", param, resp.StatusCode)
		}
	}
}

// The row read is every field the recorder wrote, and it is narrowed the same
// way a list is: another sandbox's row is not found.
func TestControlClientGetHTTPReadsTheWholeRow(t *testing.T) {
	url, key, _ := controlClientFixture(t)
	client := NewControlClient(url, key, "project-1", "pool-1", nil)
	ctx := context.Background()
	rows, err := client.ListHTTP(ctx, "sandbox-1", AuditQuery{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("list: %+v, %v", rows, err)
	}
	row, err := client.GetHTTP(ctx, "sandbox-1", rows[0].ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.ID != rows[0].ID || row.URL != rows[0].URL || row.SwappedUseIDs != rows[0].SwappedUseIDs {
		t.Fatalf("row = %+v, want the exchange the list named", row)
	}
	// Fields a list does not carry, which is the point of reading one row.
	if row.EnqueuedAt.IsZero() || row.WrittenAt.IsZero() {
		t.Fatalf("row = %+v, want the recorder's own timestamps", row)
	}
	if _, err := client.GetHTTP(ctx, "sandbox-2", rows[0].ID); !errors.Is(err, ErrAuditArtifactNotFound) {
		t.Fatalf("get scoped to another sandbox = %v, want not found", err)
	}
	if _, err := client.GetHTTP(ctx, "sandbox-1", 999999); !errors.Is(err, ErrAuditArtifactNotFound) {
		t.Fatalf("get of a row that does not exist = %v, want not found", err)
	}
}

// A SOCKS connect is a different table with its own row numbers, so an HTTP
// exchange's cursor means nothing there. Answering 200 with an arbitrary
// suffix of the SOCKS rows is the failure the HTTP-only list exists to refuse.
func TestControlSOCKSRefusesAnHTTPCursor(t *testing.T) {
	url, key, _ := controlClientFixture(t)
	client := NewControlClient(url, key, "project-1", "pool-1", nil)
	token, err := CreateControlToken(key, ControlTokenClaims{ProjectID: "project-1", WorkerID: "pool-1"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.get(context.Background(), url+"/audit/socks?after_id=http_42", token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET /audit/socks?after_id= = %d, want 400", resp.StatusCode)
	}
}
