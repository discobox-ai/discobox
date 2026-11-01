package proxy

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/discobox-ai/x/gormdb"

	"github.com/discobox-ai/discobox/proxy/internal/audit"
)

// passthroughClient is mitmClient that also trusts the origin's own
// certificate, as a client that pins its server would.
func (f *trustFixture) passthroughClient(t *testing.T, id string) *http.Client {
	t.Helper()
	client := f.clients[id]
	transport := client.Transport.(*http.Transport).Clone()
	pool := transport.TLSClientConfig.RootCAs.Clone()
	pool.AddCert(f.origin.Certificate())
	transport.TLSClientConfig.RootCAs = pool
	return &http.Client{Transport: transport}
}

func (f *trustFixture) applyPassthrough(t *testing.T, allow map[string][]string, passthrough map[string][]string) {
	t.Helper()
	cfg := f.cfg
	cfg.Allowlist = AllowlistConfig{Clients: allow}
	cfg.Passthrough = PassthroughConfig{Clients: passthrough}
	if err := f.server.ApplyConfig(cfg); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
}

func TestPassthroughSplicesTheTunnelUnreadAndAuditsIt(t *testing.T) {
	f := newTrustFixture(t, "")
	f.applyPassthrough(t, nil, map[string][]string{"sandbox-1": {"127.0.0.1"}})

	client := f.passthroughClient(t, "sandbox-1")
	resp, err := getURL(t, client, f.origin.URL)
	if err != nil {
		t.Fatalf("GET through passthrough: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "ok" {
		t.Fatalf("body = %q, want ok", body)
	}
	if got := resp.TLS.PeerCertificates[0]; !got.Equal(f.origin.Certificate()) {
		t.Fatalf("the client saw %q, not the origin's own certificate: the tunnel was intercepted", got.Subject)
	}
	client.CloseIdleConnections()

	rows := waitForTunnelRows(t, f.dsn, 2)
	open, closed := rows[0], rows[1]
	if open.Tunnel.Phase != "open" || closed.Tunnel.Phase != "close" {
		t.Fatalf("phases = %q, %q; want open, close", open.Tunnel.Phase, closed.Tunnel.Phase)
	}
	if open.Tunnel.ID == "" || open.Tunnel.ID != closed.Tunnel.ID {
		t.Fatalf("tunnel IDs = %q, %q; want one shared ID", open.Tunnel.ID, closed.Tunnel.ID)
	}
	if open.Method != http.MethodConnect || open.ClientID != "sandbox-1" || open.Status != http.StatusOK {
		t.Fatalf("open row = %s %s by %q, want a 200 CONNECT by sandbox-1", open.Method, open.Host, open.ClientID)
	}
	if open.Tunnel.Entry != "127.0.0.1" || open.Tunnel.DialedAddress == "" {
		t.Fatalf("open row entry = %q, dialed = %q", open.Tunnel.Entry, open.Tunnel.DialedAddress)
	}
	if !open.Tunnel.TLS || open.Tunnel.ALPN != "http/1.1" || open.Tunnel.TLSVersions == "" {
		t.Fatalf("open row did not record the ClientHello: %+v", open.Tunnel)
	}
	if closed.Tunnel.C2SBytes == 0 || closed.Tunnel.S2CBytes == 0 || closed.Tunnel.ClosedBy == "" {
		t.Fatalf("close row = %+v, want bytes each way and who closed", closed.Tunnel)
	}
}

func TestPassthroughIsPerClientAndOthersAreStillIntercepted(t *testing.T) {
	f := newTrustFixture(t, "")
	f.applyPassthrough(t, nil, map[string][]string{"sandbox-1": {"127.0.0.1"}})

	resp, err := getURL(t, f.passthroughClient(t, "sandbox-2"), f.origin.URL)
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		if resp.TLS.PeerCertificates[0].Equal(f.origin.Certificate()) {
			t.Fatal("sandbox-2 has no passthrough but reached the origin unread")
		}
	}
	// Intercepted, the proxy's upstream refuses the origin's private CA; what
	// matters is that no tunnel row was written for sandbox-2.
	time.Sleep(100 * time.Millisecond)
	if rows := readTunnelRows(t, f.dsn); len(rows) != 0 {
		t.Fatalf("tunnel rows = %d, want none", len(rows))
	}
}

func TestPassthroughNeverAdmitsAHost(t *testing.T) {
	f := newTrustFixture(t, "")
	f.applyPassthrough(t,
		map[string][]string{"sandbox-1": {"example.com"}},
		map[string][]string{"sandbox-1": {"127.0.0.1"}})

	if resp, err := getURL(t, f.passthroughClient(t, "sandbox-1"), f.origin.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("a host passthrough names but allow does not was reached: %d", resp.StatusCode)
	}
	blocked := waitForBlockedRows(t, f.dsn, 1)
	if blocked[0].BlockedReason != "host denied" || blocked[0].Tunnel.Phase != "" {
		t.Fatalf("blocked row = %q, tunnel %+v; want a host denial and no tunnel", blocked[0].BlockedReason, blocked[0].Tunnel)
	}
}

func TestPassthroughRecordsAFailedDial(t *testing.T) {
	f := newTrustFixture(t, "")
	f.applyPassthrough(t, nil, map[string][]string{"sandbox-1": {"127.0.0.1"}})
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := listener.Addr().String()
	_ = listener.Close()

	if resp, err := getURL(t, f.passthroughClient(t, "sandbox-1"), "https://"+closedAddr); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a dial that failed was answered")
	}
	rows := waitForTunnelRows(t, f.dsn, 1)
	if rows[0].Tunnel.Phase != "failed" || rows[0].Status != http.StatusBadGateway || rows[0].Tunnel.Error == "" {
		t.Fatalf("row = %d %+v, want a failed 502 with its error", rows[0].Status, rows[0].Tunnel)
	}
}

func TestPeekClientHelloPassesNonTLSBytesOn(t *testing.T) {
	server, client := net.Pipe()
	go func() { _, _ = client.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n")) }()
	hello, first := peekClientHello(t.Context(), server, time.Second)
	if hello != nil {
		t.Fatalf("hello = %+v, want none for a non-TLS first flight", hello)
	}
	if !bytes.HasPrefix([]byte("SSH-2.0-OpenSSH_9.6\r\n"), first) || len(first) == 0 {
		t.Fatalf("peeked bytes = %q, want the start of what the client sent", first)
	}
}

func getURL(t *testing.T, client *http.Client, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client.Do(req)
}

func readTunnelRows(t *testing.T, dsn string) []audit.HTTPExchange {
	t.Helper()
	pools, err := gormdb.Open(gormdb.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open audit db: %v", err)
	}
	defer func() { _ = pools.Close() }()
	var rows []audit.HTTPExchange
	if err := pools.Read.Where("tunnel_phase <> ''").Order("id").Find(&rows).Error; err != nil {
		t.Fatalf("read audit exchanges: %v", err)
	}
	return rows
}

func waitForTunnelRows(t *testing.T, dsn string, want int) []audit.HTTPExchange {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows := readTunnelRows(t, dsn)
		if len(rows) >= want {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("tunnel rows = %d, want %d", len(rows), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The gate is never passed through, however its name is spelled: the checks
// before the filter match it as written, and a trailing dot would otherwise
// slip past them into a passthrough "*".
func TestPassthroughNeverSplicesTheGate(t *testing.T) {
	f := newTrustFixture(t, "")
	cfg := f.cfg
	cfg.Secrets.GateHost = "gate.discobox.test"
	cfg.Passthrough = PassthroughConfig{Clients: map[string][]string{"sandbox-1": {"*"}}}
	if err := f.server.ApplyConfig(cfg); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	for _, host := range []string{"gate.discobox.test", "gate.discobox.test.", "GATE.discobox.test."} {
		if resp, err := getURL(t, f.passthroughClient(t, "sandbox-1"), "https://"+host+"/"); err == nil {
			_ = resp.Body.Close()
		}
	}
	time.Sleep(200 * time.Millisecond)
	if rows := readTunnelRows(t, f.dsn); len(rows) != 0 {
		t.Fatalf("the gate was passed through: %+v", rows[0].Tunnel)
	}
}
