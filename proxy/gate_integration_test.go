package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy/internal/audit"
	"github.com/discobox-ai/discobox/proxy/internal/secrets"
	"github.com/discobox-ai/x/gormdb"
)

const testGateHost = "api.discobox.internal"

// gateResolver answers a request itself, the way the pool's gate answers from
// the control plane. A request it is told to refuse, by a header the test
// sets, it refuses; one it is told to fail, it lets in and then fails.
type gateResolver struct {
	noGatewayHeaders
	stubResolver
	mu   sync.Mutex
	seen []string
}

func (r *gateResolver) Gate(_ context.Context, req secrets.GateRequest) (secrets.GateAdmission, error) {
	r.mu.Lock()
	r.seen = append(r.seen, req.ClientID+" "+req.Request.Method+" "+req.Request.URL.Path)
	r.mu.Unlock()
	switch req.Request.Header.Get("X-Test-Gate") {
	case "":
	case "fail":
		// Let in, and then the upstream behind the gate did not answer.
		return secrets.GateAdmission{}, errors.New("control plane reset the connection")
	default:
		return secrets.GateAdmission{}, &secrets.GateRefusal{Reason: "this pool cannot reach the control plane yet"}
	}
	body := "admitted " + req.ClientID
	return secrets.GateAdmission{Response: &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"text/plain"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req.Request,
		Proto:         "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
	}}, nil
}

// The gate host is never sent to the internet. A request is answered by the
// gate, whatever the allowlist says of the host; one the gate refuses is
// refused by the proxy and recorded once, as blocked.
func TestHTTPProxyGateHostIsAnsweredByTheGateAlone(t *testing.T) {
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
	resolver := &gateResolver{}
	dbPath := filepath.Join(dir, "audit.db")
	server, err := NewServer(ctx, Config{
		ListenAddress:             "127.0.0.1:0",
		AllowLoopbackDestinations: true,
		CertDir:                   prepared.Bundle.Dir,
		DatabaseDSN:               dbPath,
		Recording:                 RecordingConfig{Enabled: true, QueueSize: 16},
		// An allowlist that admits nothing but an unrelated host: the gate
		// host is not the allowlist's to refuse.
		Allowlist: AllowlistConfig{Clients: map[string][]string{"sandbox-1": {"only.allowed.example.com"}}},
		Secrets:   SecretsConfig{GateHost: testGateHost},
	}, prepared.Bundle, resolver)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	addr := waitForAddr(t, server)
	client := gateClient(t, addr.String(), prepared.Clients["sandbox-1"])

	call := func(gate string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+testGateHost+"/projects/default/sandboxes", nil)
		if err != nil {
			t.Fatal(err)
		}
		if gate != "" {
			req.Header.Set("X-Test-Gate", gate)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("client.Do() error = %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	if status, body := call(""); status != http.StatusOK || body != "admitted sandbox-1" {
		t.Fatalf("admitted call = %d %q, want the gate's answer for sandbox-1", status, body)
	}
	if status, body := call("refuse"); status != http.StatusForbidden || body != "blocked by proxy: this pool cannot reach the control plane yet" {
		t.Fatalf("refused call = %d %q, want a 403 carrying the gate's reason alone", status, body)
	}
	if status, body := call("fail"); status != http.StatusBadGateway || !strings.Contains(body, "unreachable") {
		t.Fatalf("failed call = %d %q, want a 502 saying the API was unreachable", status, body)
	}
	resolver.mu.Lock()
	seen := append([]string(nil), resolver.seen...)
	resolver.mu.Unlock()
	if len(seen) != 3 || seen[0] != "sandbox-1 GET /projects/default/sandboxes" {
		t.Fatalf("gate saw %q, want every call, from the sandbox that made it", seen)
	}

	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("ListenAndServe() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for proxy shutdown")
	}
	pools, err := gormdb.Open(gormdb.Config{DSN: dbPath})
	if err != nil {
		t.Fatalf("open audit db: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	var exchanges []audit.HTTPExchange
	if err := pools.Read.Where("client_id = ?", "sandbox-1").Order("id").Find(&exchanges).Error; err != nil {
		t.Fatalf("read audit exchanges: %v", err)
	}
	var blocked, answered, failed int
	for _, exchange := range exchanges {
		switch {
		case exchange.Blocked && strings.HasPrefix(exchange.BlockedReason, "gate: "):
			blocked++
		case !exchange.Blocked && exchange.Status == http.StatusOK:
			answered++
		case !exchange.Blocked && exchange.Status == http.StatusBadGateway:
			// Let in and then failed: it may have been acted on, so it is
			// no refusal.
			failed++
		default:
			t.Fatalf("unexpected audit row %+v", exchange)
		}
	}
	if blocked != 1 || answered != 1 || failed != 1 {
		t.Fatalf("audit = %d blocked, %d answered, %d failed; want each refusal once, the admitted call once, and the failed call once, as no refusal", blocked, answered, failed)
	}
}

// gateClient trusts the proxy's mTLS CA and its MITM CA, as a sandbox does.
func gateClient(t *testing.T, addr string, material ClientMaterial) *http.Client {
	t.Helper()
	clientCert, err := tls.LoadX509KeyPair(material.ClientCertPath, material.ClientKeyPath)
	if err != nil {
		t.Fatalf("load client cert: %v", err)
	}
	pool := x509.NewCertPool()
	for _, path := range []string{material.MTLSCAPath, material.MITMCAPath} {
		pem, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read CA %s: %v", path, err)
		}
		if !pool.AppendCertsFromPEM(bytes.TrimSpace(pem)) {
			t.Fatalf("parse CA %s", path)
		}
	}
	proxyURL, err := url.Parse("https://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{clientCert},
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{"http/1.1"},
		},
	}}
}
