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

// gateResolver admits a request carrying "Bearer live-use" and answers it
// itself, the way the pool's gate answers from the control plane.
type gateResolver struct {
	stubResolver
	mu   sync.Mutex
	seen []string
}

func (r *gateResolver) Gate(_ context.Context, req secrets.GateRequest) (secrets.GateAdmission, error) {
	r.mu.Lock()
	r.seen = append(r.seen, req.ClientID+" "+req.Request.Method+" "+req.Request.URL.Path)
	r.mu.Unlock()
	switch req.Request.Header.Get("Authorization") {
	case "Bearer live-use":
	case "Bearer failing-use":
		// Let in, and then the upstream behind the gate did not answer.
		return secrets.GateAdmission{UseID: "use_failed"}, errors.New("control plane reset the connection")
	case "Bearer judged-use":
		// A use the gate recognized and the judge then refused.
		return secrets.GateAdmission{}, &secrets.GateRefusal{Reason: "not what use_judged was approved for", UseID: "use_judged"}
	default:
		return secrets.GateAdmission{}, &secrets.GateRefusal{Reason: "no live use of ai.discobox.sandbox"}
	}
	body := "admitted " + req.ClientID
	return secrets.GateAdmission{UseID: "use_live", Response: &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"text/plain"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req.Request,
		Proto:         "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
	}}, nil
}

// The gate host is never sent to the internet. A request carrying a live use
// is answered by the gate, whatever the allowlist says of the host; anything
// else is refused by the proxy and recorded once, as blocked.
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
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   dbPath,
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16},
		// An allowlist that admits nothing but an unrelated host: the gate
		// host is not the allowlist's to refuse.
		Allowlist: AllowlistConfig{Enabled: true, Domains: []string{"only.allowed.example.com"}},
		Secrets:   SecretsConfig{GateHost: testGateHost},
	}, prepared.Bundle, resolver)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	addr := waitForAddr(t, server)
	client := gateClient(t, addr.String(), prepared.Clients["sandbox-1"])

	call := func(token string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+testGateHost+"/projects/default/sandboxes", nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("client.Do() error = %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	if status, body := call("live-use"); status != http.StatusOK || body != "admitted sandbox-1" {
		t.Fatalf("admitted call = %d %q, want the gate's answer for sandbox-1", status, body)
	}
	if status, body := call("not-a-use"); status != http.StatusForbidden || body != "blocked by proxy: no live use of ai.discobox.sandbox" {
		t.Fatalf("refused call = %d %q, want a 403 carrying the gate's reason alone", status, body)
	}
	if status, _ := call(""); status != http.StatusForbidden {
		t.Fatalf("call with nothing = %d, want 403", status)
	}
	if status, _ := call("judged-use"); status != http.StatusForbidden {
		t.Fatalf("judged call = %d, want 403", status)
	}
	if status, body := call("failing-use"); status != http.StatusBadGateway || !strings.Contains(body, "unreachable") {
		t.Fatalf("failed call = %d %q, want a 502 saying the API was unreachable", status, body)
	}
	resolver.mu.Lock()
	seen := append([]string(nil), resolver.seen...)
	resolver.mu.Unlock()
	if len(seen) != 5 || seen[0] != "sandbox-1 GET /projects/default/sandboxes" {
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
	// Every row says which use it was, where there was one: the admitted
	// call's, and the refused call that carried a use the gate recognized.
	var blocked, answered, failed int
	for _, exchange := range exchanges {
		switch {
		case exchange.Blocked && strings.HasPrefix(exchange.BlockedReason, "gate: not what use_judged"):
			blocked++
			if exchange.SwappedUseIDs != "use_judged" {
				t.Fatalf("judged row uses = %q, want the use it carried", exchange.SwappedUseIDs)
			}
		case exchange.Blocked && strings.HasPrefix(exchange.BlockedReason, "gate: "):
			blocked++
			if exchange.SwappedUseIDs != "" {
				t.Fatalf("refused row uses = %q, want none: it carried no use", exchange.SwappedUseIDs)
			}
		case !exchange.Blocked && exchange.Status == http.StatusOK:
			answered++
			if exchange.SwappedUseIDs != "use_live" {
				t.Fatalf("admitted row uses = %q, want the use it was let in under", exchange.SwappedUseIDs)
			}
		case !exchange.Blocked && exchange.Status == http.StatusBadGateway:
			// Let in and then failed: it may have been acted on, so it is
			// no refusal, and it keeps the use it went under.
			failed++
			if exchange.SwappedUseIDs != "use_failed" {
				t.Fatalf("failed row uses = %q, want the use it was let in under", exchange.SwappedUseIDs)
			}
		default:
			t.Fatalf("unexpected audit row %+v", exchange)
		}
	}
	if blocked != 3 || answered != 1 || failed != 1 {
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
