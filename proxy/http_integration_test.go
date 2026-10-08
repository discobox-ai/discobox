package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy/bridge"
	"github.com/discobox-ai/discobox/proxy/internal/audit"
	"github.com/discobox-ai/discobox/proxy/internal/secrets"
	"github.com/discobox-ai/x/gormdb"
)

type stubResolver struct {
	value string
	host  string
	useID string
	// secretID is the secret every value is, as the control plane names it.
	secretID string
	// reports is where this resolver is told what the upstream made of its
	// value, for the tests that assert on it. Nil discards.
	reports *reportLog
	// deny, when set, is the reason every request is refused with; empty
	// allows them all. judged, when set, collects what the authorizer was
	// shown.
	deny   string
	judged *[]secrets.AuthorizeRequest
	// refuse, when set, is how a refusal says no in the request's own
	// protocol.
	refuse func(context.Context, string) (secrets.Refusal, bool)
	// captured, when set, is where the authorizer puts the body it read, the
	// way a judge that asked to see it does.
	captured *[]byte
	// resolved, when set, counts the sentinels this resolver was asked to
	// resolve. A refused request must leave it at zero.
	resolved *atomic.Int64
}

func TestMergeResponseBodyErrorRecordsUnexpectedEOF(t *testing.T) {
	if got := mergeResponseBodyError("", io.ErrUnexpectedEOF); got != "unexpected EOF" {
		t.Fatalf("mergeResponseBodyError() = %q, want unexpected EOF", got)
	}
	if got := mergeResponseBodyError("disk full", io.ErrUnexpectedEOF); got != "disk full; response read: unexpected EOF" {
		t.Fatalf("mergeResponseBodyError() with spool error = %q", got)
	}
	if got := mergeResponseBodyError("", io.EOF); got != "" {
		t.Fatalf("mergeResponseBodyError() recorded clean EOF as %q", got)
	}
}

// Report satisfies the resolver contract for the tests that do not care what
// the upstream made of the value; the ones that do use reportLog.
func (r stubResolver) Report(_ context.Context, req secrets.ReportRequest) error {
	if r.reports != nil {
		return r.reports.Report(context.Background(), req)
	}
	return nil
}

// Gate admits nothing: these tests have no gate host.
func (stubResolver) Gate(context.Context, secrets.GateRequest) (secrets.GateAdmission, error) {
	return secrets.GateAdmission{}, &secrets.GateRefusal{Reason: "no gate here"}
}

func (r stubResolver) Authorize(ctx context.Context, req secrets.AuthorizeRequest) (secrets.Verdict, error) {
	if r.judged != nil {
		*r.judged = append(*r.judged, req)
	}
	if r.captured != nil {
		data, _, err := req.Body.Capture(ctx)
		if err != nil {
			return secrets.Verdict{}, err
		}
		*r.captured = append([]byte(nil), data...)
	}
	verdict := secrets.Verdict{Allow: r.deny == "", Reason: r.deny}
	if !verdict.Allow {
		verdict.Refuse = r.refuse
	}
	if r.useID != "" {
		verdict.UseIDs = []string{r.useID}
	}
	return verdict, nil
}

func (r stubResolver) Resolve(_ context.Context, req secrets.ResolveRequest) (secrets.ResolveResult, error) {
	if r.resolved != nil {
		r.resolved.Add(1)
	}
	if r.host != "" && req.Host != r.host {
		return secrets.ResolveResult{}, secrets.ErrDenied
	}
	return secrets.ResolveResult{Value: r.value, UseID: r.useID, SecretID: r.secretID, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func TestHTTPProxyMTLSIdentityHeaderRewriteAndAudit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var sawAuthorization string
	var sawInjectedSecret string
	origin := newOrigin(func(w http.ResponseWriter, r *http.Request) {
		sawAuthorization = r.Header.Get("Authorization")
		sawInjectedSecret = r.Header.Get("X-Injected-Secret")
		_, _ = io.WriteString(w, "ok")
	})
	defer origin.Close()

	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	originHost := originURL.Hostname()

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
		Headers: []HeaderRule{{
			ID:      "origin-auth",
			Pattern: originHost,
			Set: map[string]string{
				"Authorization":        "Bearer injected",
				"X-Injected-Secret":    "super-secret",
				"X-Injected-Nonsecret": "non-secret",
			},
		}},
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
	proxyURL, err := url.Parse("https://" + addr.String())
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			RootCAs:      caPool,
			Certificates: []tls.Certificate{clientCert},
			MinVersion:   tls.VersionTLS12,
		},
	}}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Get() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if sawAuthorization != "Bearer injected" {
		t.Fatalf("Authorization = %q", sawAuthorization)
	}
	if sawInjectedSecret != "super-secret" {
		t.Fatalf("X-Injected-Secret = %q", sawInjectedSecret)
	}

	closeServer()

	pools, err := gormdb.Open(gormdb.Config{DSN: dbPath})
	if err != nil {
		t.Fatalf("open audit db: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	var exchange audit.HTTPExchange
	if err := pools.Read.Where("client_id = ?", "sandbox-1").First(&exchange).Error; err != nil {
		t.Fatalf("read audit exchange: %v", err)
	}
	if exchange.AppliedRuleID != "origin-auth" {
		t.Fatalf("AppliedRuleID = %q", exchange.AppliedRuleID)
	}
	if exchange.Status != http.StatusOK {
		t.Fatalf("audit status = %d", exchange.Status)
	}
	if strings.Contains(exchange.RequestHeaders, "Bearer injected") || strings.Contains(exchange.RequestHeaders, "super-secret") || strings.Contains(exchange.RequestHeaders, "non-secret") {
		t.Fatalf("audit request headers leaked injected values: %s", exchange.RequestHeaders)
	}
	var headers map[string][]string
	if err := json.Unmarshal([]byte(exchange.RequestHeaders), &headers); err != nil {
		t.Fatalf("unmarshal request headers: %v", err)
	}
	for _, header := range []string{"Authorization", "X-Injected-Secret", "X-Injected-Nonsecret"} {
		values := headers[header]
		if len(values) != 1 || values[0] != "[REDACTED]" {
			t.Fatalf("%s audit values = %#v", header, values)
		}
	}
}

// A swapped request's row names the secret it spent whatever stood in for it:
// an ordinary injected sentinel, which has no approved use (ADR 26-10-01-240),
// and one taken under a use, which names the use as well (ADR 0130 §3).
func TestHTTPProxySecretSentinelSwapAndAudit(t *testing.T) {
	t.Run("ordinary sentinel", func(t *testing.T) { testSentinelSwapAndAudit(t, "") })
	t.Run("approved use", func(t *testing.T) { testSentinelSwapAndAudit(t, "use_abc") })
}

func testSentinelSwapAndAudit(t *testing.T, useID string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"
	const realValue = "sk-ant-oat01-REALSECRETVALUE1234567890abcdefgh"

	var sawAuthorization string
	origin := newOrigin(func(w http.ResponseWriter, r *http.Request) {
		sawAuthorization = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	})
	defer origin.Close()

	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	originHost := originURL.Hostname()

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
		Secrets: SecretsConfig{
			Clients: []SecretClient{{
				ClientID:  "sandbox-1",
				Sentinels: []string{sentinel},
			}},
		},
	}, prepared.Bundle, stubResolver{value: realValue, host: originHost, useID: useID, secretID: "sec_abc"})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
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

	client := mtlsHTTPClient(t, addr.String(), prepared.Clients["sandbox-1"])
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sentinel)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if sawAuthorization != "Bearer "+realValue {
		t.Fatalf("upstream Authorization = %q, want swapped real value", sawAuthorization)
	}

	closeServer()

	pools, err := gormdb.Open(gormdb.Config{DSN: dbPath})
	if err != nil {
		t.Fatalf("open audit db: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	var exchange audit.HTTPExchange
	if err := pools.Read.Where("client_id = ?", "sandbox-1").First(&exchange).Error; err != nil {
		t.Fatalf("read audit exchange: %v", err)
	}
	if strings.Contains(exchange.RequestHeaders, realValue) {
		t.Fatalf("audit leaked real secret value: %s", exchange.RequestHeaders)
	}
	var headers map[string][]string
	if err := json.Unmarshal([]byte(exchange.RequestHeaders), &headers); err != nil {
		t.Fatalf("unmarshal request headers: %v", err)
	}
	if values := headers["Authorization"]; len(values) != 1 || values[0] != "[REDACTED]" {
		t.Fatalf("Authorization audit values = %#v, want [REDACTED]", values)
	}
	// The join to the control plane's verdict trail (ADR 0130 §3): the row
	// names the approved use the request spent, and never the sentinel.
	if exchange.SwappedUseIDs != useID {
		t.Fatalf("SwappedUseIDs = %q, want %q", exchange.SwappedUseIDs, useID)
	}
	if strings.Contains(exchange.SwappedUseIDs, sentinel) {
		t.Fatalf("audit recorded a sentinel in the use ID column: %s", exchange.SwappedUseIDs)
	}
	// And the secret it spent, whether or not there was a use to name.
	if exchange.SwappedSecretIDs != "sec_abc" {
		t.Fatalf("SwappedSecretIDs = %q, want sec_abc", exchange.SwappedSecretIDs)
	}
}

// A judge that reads the body changes nothing about what is sent: the upstream
// receives every byte the sandbox sent, in order, including the part past what
// the judge could be shown (ADR 26-09-22-838 §6).
func TestHTTPProxySendsTheBodyTheJudgeRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"
	const realValue = "sk-ant-oat01-REALSECRETVALUE1234567890abcdefgh"
	sent := bytes.Repeat([]byte(`{"title":"a pull request","body":"words"},`), 2*secrets.MaxCapturedBody/40)

	var arrived []byte
	var sawAuthorization string
	origin := newOrigin(func(w http.ResponseWriter, r *http.Request) {
		sawAuthorization = r.Header.Get("Authorization")
		arrived, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, "ok")
	})
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}

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
	var captured []byte
	server, err := NewServer(ctx, Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   filepath.Join(dir, "audit.db"),
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16},
		Secrets: SecretsConfig{Clients: []SecretClient{{
			ClientID:  "sandbox-1",
			Sentinels: []string{sentinel},
		}}},
	}, prepared.Bundle, stubResolver{value: realValue, host: originURL.Hostname(), useID: "use_abc", captured: &captured})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
		<-errCh
	})
	addr := waitForAddr(t, server)

	client := mtlsHTTPClient(t, addr.String(), prepared.Clients["sandbox-1"])
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin.URL+"/repos/org/repo/pulls", bytes.NewReader(sent))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sentinel)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the upstream's 200", resp.StatusCode)
	}
	if !bytes.Equal(captured, sent[:secrets.MaxCapturedBody]) {
		t.Fatalf("the judge read %d bytes, want the first %d of the body", len(captured), secrets.MaxCapturedBody)
	}
	if !bytes.Equal(arrived, sent) {
		t.Fatalf("the upstream received %d bytes, want the %d the sandbox sent", len(arrived), len(sent))
	}
	if sawAuthorization != "Bearer "+realValue {
		t.Fatalf("upstream Authorization = %q, want the credential swapped in", sawAuthorization)
	}
}

// A request the judge does not allow is answered by the proxy and never sent,
// with the credential it would have carried going nowhere: not upstream, not
// into the judge's view of the request, and not into the audit row that
// records the refusal.
//
// It is also never fetched. Authorization runs before resolution (ADR 26-09-22-838
// §4), so a refusal is a credential that stayed where it was rather than one
// that was retrieved and then withheld.
func TestHTTPProxyJudgeRefusesASwappedRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"
	const realValue = "sk-ant-oat01-REALSECRETVALUE1234567890abcdefgh"
	const reason = "deleting a repository is not what use_abc was approved for"

	reached := false
	origin := newOrigin(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = io.WriteString(w, "ok")
	})
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}

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
	var judged []secrets.AuthorizeRequest
	var resolved atomic.Int64
	dbPath := filepath.Join(dir, "audit.db")
	server, err := NewServer(ctx, Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   dbPath,
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16},
		Secrets: SecretsConfig{Clients: []SecretClient{{
			ClientID:  "sandbox-1",
			Sentinels: []string{sentinel},
		}}},
	}, prepared.Bundle, stubResolver{value: realValue, host: originURL.Hostname(), useID: "use_abc", deny: reason, judged: &judged, resolved: &resolved})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
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

	client := mtlsHTTPClient(t, addr.String(), prepared.Clients["sandbox-1"])
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, origin.URL+"/repos/org/repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sentinel)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), reason) {
		t.Fatalf("response = %d %q, want 403 carrying the judge's reason", resp.StatusCode, body)
	}
	if reached {
		t.Fatal("a request the judge refused reached the upstream")
	}

	if len(judged) != 1 {
		t.Fatalf("judged %d requests, want 1", len(judged))
	}
	shown := judged[0]
	if shown.ClientID != "sandbox-1" || shown.Method != http.MethodDelete ||
		len(shown.Sentinels) != 1 || shown.Sentinels[0] != sentinel {
		t.Fatalf("judge was shown %+v, want the sandbox, the method, and the sentinel it carried", shown)
	}
	if got := shown.Header.Get("Authorization"); got != "Bearer "+sentinel {
		t.Fatalf("judge saw Authorization %q, want the sentinel and never the credential", got)
	}
	if got := resolved.Load(); got != 0 {
		t.Fatalf("a refused request resolved %d credentials, want none fetched at all", got)
	}

	closeServer()
	pools, err := gormdb.Open(gormdb.Config{DSN: dbPath})
	if err != nil {
		t.Fatalf("open audit db: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	var exchanges []audit.HTTPExchange
	if err := pools.Read.Where("client_id = ?", "sandbox-1").Find(&exchanges).Error; err != nil {
		t.Fatalf("read audit exchanges: %v", err)
	}
	blocked := 0
	for _, exchange := range exchanges {
		row, err := json.Marshal(exchange)
		if err != nil {
			t.Fatalf("marshal audit row: %v", err)
		}
		if strings.Contains(string(row), realValue) {
			t.Fatalf("audit leaked the real secret value: %s", row)
		}
		if exchange.Blocked {
			blocked++
			if exchange.BlockedReason != "judge: "+reason || exchange.Status != http.StatusForbidden {
				t.Fatalf("blocked row = %+v, want the judge's reason and a 403", exchange)
			}
			// A refusal names the use it was about, though this assertion
			// alone does not say where the name came from: the row carried the
			// use before this change too, off the swap rather than off the
			// verdict. What proves the new path is resolved.Load() above.
			if exchange.SwappedUseIDs != "use_abc" {
				t.Fatalf("blocked row named use %q, want the use the request was refused under", exchange.SwappedUseIDs)
			}
		}
	}
	if len(exchanges) != 1 || blocked != 1 {
		t.Fatalf("audit rows = %d (%d blocked), want the refusal recorded once", len(exchanges), blocked)
	}
}

// A refusal the request's own protocol has a way to say is said that way, so a
// client that never shows a 403's body shows why (ADR 26-09-26-240 §5). It is
// the same refusal, recorded as blocked, with the status that was sent.
func TestHTTPProxyAnswersARefusalInTheRequestsOwnProtocol(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"
	const reason = "the use is for another branch"
	origin := newOrigin(func(http.ResponseWriter, *http.Request) {
		t.Error("a refused request reached the upstream")
	})
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
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
	var said string
	dbPath := filepath.Join(dir, "audit.db")
	server, err := NewServer(ctx, Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   dbPath,
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16},
		Secrets: SecretsConfig{Clients: []SecretClient{{
			ClientID:  "sandbox-1",
			Sentinels: []string{sentinel},
		}}},
	}, prepared.Bundle, stubResolver{value: "real", host: originURL.Hostname(), useID: "use_abc", deny: reason,
		refuse: func(_ context.Context, sentence string) (secrets.Refusal, bool) {
			said = sentence
			return secrets.Refusal{Status: http.StatusOK, ContentType: "application/x-git-receive-pack-result", Body: []byte("0000")}, true
		}})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	addr := waitForAddr(t, server)

	client := mtlsHTTPClient(t, addr.String(), prepared.Clients["sandbox-1"])
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin.URL+"/org/repo.git/git-receive-pack", strings.NewReader("push"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sentinel)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/x-git-receive-pack-result" || string(body) != "0000" {
		t.Fatalf("response = %d %q %q, want the protocol's own refusal", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if said != "blocked by proxy: "+reason {
		t.Fatalf("the refusal was written from %q, want the proxy's own sentence", said)
	}

	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("ListenAndServe() error = %v", err)
	}
	pools, err := gormdb.Open(gormdb.Config{DSN: dbPath})
	if err != nil {
		t.Fatalf("open audit db: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	var exchanges []audit.HTTPExchange
	if err := pools.Read.Where("client_id = ?", "sandbox-1").Find(&exchanges).Error; err != nil {
		t.Fatalf("read audit exchanges: %v", err)
	}
	if len(exchanges) != 1 || !exchanges[0].Blocked || exchanges[0].BlockedReason != "judge: "+reason || exchanges[0].Status != http.StatusOK {
		t.Fatalf("audit rows = %+v, want one blocked row with the judge's reason and the status sent", exchanges)
	}
}

// A request the proxy refuses is its own answer, recorded once as blocked. The
// response path must not record it a second time as an ordinary 403, which
// would read as the upstream having refused it.
func TestHTTPProxyRecordsAHostItRefusesOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reached := false
	origin := newOrigin(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = io.WriteString(w, "ok")
	})
	defer origin.Close()

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
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16},
		Allowlist:     AllowlistConfig{Enabled: true, Domains: []string{"only.allowed.example.com"}},
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	addr := waitForAddr(t, server)

	client := mtlsHTTPClient(t, addr.String(), prepared.Clients["sandbox-1"])
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || reached {
		t.Fatalf("status = %d, reached = %v; want the proxy to refuse it", resp.StatusCode, reached)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("ListenAndServe() error = %v", err)
	}

	pools, err := gormdb.Open(gormdb.Config{DSN: dbPath})
	if err != nil {
		t.Fatalf("open audit db: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	var exchanges []audit.HTTPExchange
	if err := pools.Read.Where("client_id = ?", "sandbox-1").Find(&exchanges).Error; err != nil {
		t.Fatalf("read audit exchanges: %v", err)
	}
	if len(exchanges) != 1 || !exchanges[0].Blocked || exchanges[0].BlockedReason != "host denied" {
		t.Fatalf("audit rows = %+v, want the refusal recorded once, as blocked", exchanges)
	}
}

func TestHTTPProxySecretSentinelDeniedForOtherHost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"
	const realValue = "sk-ant-oat01-REALSECRETVALUE1234567890abcdefgh"

	var sawAuthorization string
	origin := newOrigin(func(w http.ResponseWriter, r *http.Request) {
		sawAuthorization = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	})
	defer origin.Close()

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

	server, err := NewServer(ctx, Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   filepath.Join(dir, "audit.db"),
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16},
		Secrets: SecretsConfig{
			Clients: []SecretClient{{ClientID: "sandbox-1", Sentinels: []string{sentinel}}},
		},
	}, prepared.Bundle, stubResolver{value: realValue, host: "only.allowed.example.com"})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	t.Cleanup(func() { _ = server.Close(); <-errCh })
	addr := waitForAddr(t, server)

	client := mtlsHTTPClient(t, addr.String(), prepared.Clients["sandbox-1"])
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sentinel)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	defer resp.Body.Close()
	if sawAuthorization != "Bearer "+sentinel {
		t.Fatalf("upstream Authorization = %q, want unswapped sentinel (host denied)", sawAuthorization)
	}
}

func TestHTTPProxyCapturesFullBodies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const requestBody = "request body with full payload"
	const responseBody = "response body with full payload"
	origin := newOrigin(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if string(got) != requestBody {
			http.Error(w, "unexpected request body", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, responseBody)
	})
	defer origin.Close()

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
	bodyDir := filepath.Join(dir, "bodies")
	server, err := NewServer(ctx, Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   dbPath,
		Recording: RecordingConfig{
			Enabled:   true,
			QueueSize: 16,
			BodyDir:   bodyDir,
		},
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()
	closeServer := closeProxyServer(t, server, errCh)
	t.Cleanup(closeServer)
	addr := waitForAddr(t, server)

	client := mtlsHTTPClient(t, addr.String(), prepared.Clients["sandbox-1"])
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin.URL, strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	gotResponse, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(gotResponse) != responseBody {
		t.Fatalf("response status/body = %d %q", resp.StatusCode, gotResponse)
	}

	exchange := waitForHTTPExchange(t, dbPath, "client_id = ? AND method = ?", "sandbox-1", http.MethodPost)
	if exchange.RequestBodyBytes != int64(len(requestBody)) || exchange.ResponseBodyBytes != int64(len(responseBody)) {
		t.Fatalf("body bytes request=%d response=%d", exchange.RequestBodyBytes, exchange.ResponseBodyBytes)
	}
	if exchange.RequestBodyFormat != audit.BodyFormatRaw || exchange.ResponseBodyFormat != audit.BodyFormatRaw {
		t.Fatalf("body formats request=%q response=%q", exchange.RequestBodyFormat, exchange.ResponseBodyFormat)
	}
	assertSpoolFile(t, filepath.Join(bodyDir, filepath.FromSlash(exchange.RequestBodyFile)), requestBody)
	assertSpoolFile(t, filepath.Join(bodyDir, filepath.FromSlash(exchange.ResponseBodyFile)), responseBody)

	for _, tc := range []struct {
		path string
		want string
	}{
		{path: "request-body", want: requestBody},
		{path: "response-body", want: responseBody},
	} {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/audit/http/"+exchange.ID.String()+"/"+tc.path+"?client_id=sandbox-1", nil)
		rec := httptest.NewRecorder()
		server.ControlHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("control %s status = %d body=%q", tc.path, rec.Code, rec.Body.String())
		}
		if rec.Body.String() != tc.want {
			t.Fatalf("control %s body = %q", tc.path, rec.Body.String())
		}
	}

	// The same bodies through ControlClient, the reader the pool agent relays
	// with: a scoped read of the owning sandbox streams them, another sandbox's
	// scope reads them as not found.
	control := httptest.NewServer(server.ControlHandler())
	defer control.Close()
	_, readerKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	reader := NewControlClient(control.URL, readerKey, "", "", nil)
	for artifact, want := range map[string]string{AuditArtifactRequestBody: requestBody, AuditArtifactResponseBody: responseBody} {
		opened, err := reader.OpenHTTPArtifact(ctx, "sandbox-1", exchange.ID, artifact)
		if err != nil {
			t.Fatalf("OpenHTTPArtifact(%s) error = %v", artifact, err)
		}
		got, err := io.ReadAll(opened.Body)
		opened.Body.Close()
		if err != nil || string(got) != want || opened.Format != audit.BodyFormatRaw {
			t.Fatalf("OpenHTTPArtifact(%s) = %q format %q, %v; want the recorded body", artifact, got, opened.Format, err)
		}
		if _, err := reader.OpenHTTPArtifact(ctx, "sandbox-2", exchange.ID, artifact); !errors.Is(err, ErrAuditArtifactNotFound) {
			t.Fatalf("OpenHTTPArtifact(%s) scoped to another sandbox = %v, want not found", artifact, err)
		}
	}

	// The other half of the narrowing chain: the middleware pins client_id to
	// the token's sandbox, and this is what that pinning buys — a client_id
	// naming another sandbox does not reach the spooled body, it 404s. Without
	// it, narrowing would be pinning a parameter nothing enforced.
	for _, artifact := range []string{"request-body", "response-body"} {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet,
			"/audit/http/"+exchange.ID.String()+"/"+artifact+"?client_id=sandbox-2", nil)
		rec := httptest.NewRecorder()
		server.ControlHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s for another sandbox: status = %d body=%q, want 404", artifact, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), requestBody) || strings.Contains(rec.Body.String(), responseBody) {
			t.Fatalf("%s for another sandbox leaked a spooled body: %q", artifact, rec.Body.String())
		}
	}
}

// TestHTTPProxyRecordsCachedResponseBodyByReference is ADR 26-10-08-698 §1: a
// response the cache stores or serves is recorded as a reference to the cache
// entry rather than spooled beside it, reads back through the control API all
// the same, and reads as reclaimed once the cache has let the entry go.
func TestHTTPProxyRecordsCachedResponseBodyByReference(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const responseBody = "cached response body"
	var originHits int
	origin := newOrigin(func(w http.ResponseWriter, _ *http.Request) {
		originHits++
		_, _ = io.WriteString(w, responseBody)
	})
	defer origin.Close()

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
	bodyDir := filepath.Join(dir, "bodies")
	server, err := NewServer(ctx, Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   dbPath,
		Cache: CacheConfig{
			Enabled:      true,
			Dir:          filepath.Join(dir, "cache"),
			MaxSizeBytes: 1024 * 1024,
			Patterns:     []string{"/cached"},
		},
		Recording: RecordingConfig{
			Enabled:   true,
			QueueSize: 16,
			BodyDir:   bodyDir,
		},
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()
	closeServer := closeProxyServer(t, server, errCh)
	t.Cleanup(closeServer)
	addr := waitForAddr(t, server)

	client := mtlsHTTPClient(t, addr.String(), prepared.Clients["sandbox-1"])
	for range 2 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL+"/cached", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("client.Get() error = %v", err)
		}
		got, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		if resp.StatusCode != http.StatusOK || string(got) != responseBody {
			t.Fatalf("response status/body = %d %q", resp.StatusCode, got)
		}
	}
	if originHits != 1 {
		t.Fatalf("originHits = %d, want 1", originHits)
	}

	for _, column := range []string{"cache_stored", "cache_hit"} {
		exchange := waitForHTTPExchange(t, dbPath, "client_id = ? AND "+column+" = ?", "sandbox-1", true)
		if exchange.ResponseBodyFormat != audit.BodyFormatCache || exchange.ResponseBodyFile != "" ||
			exchange.ResponseBodyBytes != int64(len(responseBody)) || exchange.ResponseBodyError != "" {
			t.Fatalf("%s row body = format %q file %q bytes %d error %q, want a %d-byte reference to the cache",
				column, exchange.ResponseBodyFormat, exchange.ResponseBodyFile, exchange.ResponseBodyBytes, exchange.ResponseBodyError, len(responseBody))
		}
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/audit/http/"+exchange.ID.String()+"/response-body?client_id=sandbox-1", nil)
		rec := httptest.NewRecorder()
		server.ControlHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != responseBody {
			t.Fatalf("control response-body for the %s row = %d %q, want the cached body", column, rec.Code, rec.Body.String())
		}
	}
	// Neither exchange wrote a copy of its own.
	if entries, err := os.ReadDir(filepath.Join(bodyDir, "bodies", "sandbox-1")); err == nil && len(entries) > 0 {
		t.Fatalf("body spool holds %d files, want none for cached responses", len(entries))
	}

	// The cache bounds itself: once the entry is gone the row still says the
	// body was recorded, and the read answers that it was reclaimed.
	if err := os.RemoveAll(filepath.Join(dir, "cache")); err != nil {
		t.Fatal(err)
	}
	exchange := waitForHTTPExchange(t, dbPath, "client_id = ? AND cache_hit = ?", "sandbox-1", true)
	control := httptest.NewServer(server.ControlHandler())
	defer control.Close()
	_, readerKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	reader := NewControlClient(control.URL, readerKey, "", "", nil)
	if _, err := reader.OpenHTTPArtifact(ctx, "sandbox-1", exchange.ID, AuditArtifactResponseBody); !errors.Is(err, ErrAuditArtifactReclaimed) {
		t.Fatalf("OpenHTTPArtifact() after eviction = %v, want reclaimed", err)
	}
}

func TestHTTPProxyUpgradeAudit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	originErrCh := make(chan error, 1)
	origin := newOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "missing upgrade", http.StatusBadRequest)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = rw.Flush()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(rw, buf); err != nil {
			originErrCh <- err
			return
		}
		originErrCh <- nil
		_, _ = rw.WriteString("pong")
		_ = rw.Flush()
	})
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	prepared, err := PrepareCertificates(PrepareOptions{
		Dir:         filepath.Join(dir, "certs"),
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
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16, StreamDir: filepath.Join(dir, "streams")},
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()
	closeServer := closeProxyServer(t, server, errCh)
	t.Cleanup(closeServer)
	addr := waitForAddr(t, server)

	conn := dialProxyMTLS(ctx, t, addr.String(), prepared.Clients["sandbox-1"])
	reader := bufio.NewReader(conn)
	_, err = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", origin.URL, originURL.Host)
	if err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write upgraded bytes: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(reader, got); err != nil {
		select {
		case originErr := <-originErrCh:
			t.Fatalf("read upgraded bytes: %v; origin read: %v", err, originErr)
		default:
		}
		t.Fatalf("read upgraded bytes: %v", err)
	}
	if string(got) != "pong" {
		t.Fatalf("upgrade response = %q", got)
	}
	_ = conn.Close()
	exchange := waitForHTTPExchange(t, dbPath, "client_id = ? AND upgrade = ?", "sandbox-1", true)
	if exchange.UpgradeType != "websocket" {
		t.Fatalf("UpgradeType = %q", exchange.UpgradeType)
	}
	if exchange.UpgradeC2SBytes < 4 || exchange.UpgradeS2CBytes < 4 {
		t.Fatalf("upgrade bytes c2s=%d s2c=%d", exchange.UpgradeC2SBytes, exchange.UpgradeS2CBytes)
	}
	if exchange.StreamFile == "" || exchange.StreamFormat != audit.UpgradeStreamFormatRawFrames {
		t.Fatalf("stream metadata file=%q format=%q", exchange.StreamFile, exchange.StreamFormat)
	}
	streamPath := filepath.Join(dir, "streams", filepath.FromSlash(exchange.StreamFile))
	streamBytes := waitForStreamSpool(t, streamPath, []byte("ping"), []byte("pong"))
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/audit/http/"+exchange.ID.String()+"/stream?client_id=sandbox-1", nil)
	rec := httptest.NewRecorder()
	server.ControlHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("control stream status = %d body=%q", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), streamBytes) {
		t.Fatal("control stream response did not match spool file")
	}

	// Same row, another sandbox: a 404 here is scoping, not a missing spool.
	// The fetch above proves the stream is there and readable, so this is the
	// stream half of the narrowing chain — the body routes are pinned the same
	// way in TestHTTPProxyCapturesFullBodies.
	scoped := httptest.NewRequestWithContext(ctx, http.MethodGet, "/audit/http/"+exchange.ID.String()+"/stream?client_id=sandbox-2", nil)
	scopedRec := httptest.NewRecorder()
	server.ControlHandler().ServeHTTP(scopedRec, scoped)
	if scopedRec.Code != http.StatusNotFound {
		t.Fatalf("stream for another sandbox: status = %d body=%q, want 404", scopedRec.Code, scopedRec.Body.String())
	}
	if bytes.Contains(scopedRec.Body.Bytes(), streamBytes) {
		t.Fatalf("stream for another sandbox leaked the spool: %q", scopedRec.Body.String())
	}
}

func TestLocalForwarderHTTPToWorkerProxy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	origin := newOrigin(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	defer origin.Close()

	dir := t.TempDir()
	prepared, err := PrepareCertificates(PrepareOptions{
		Dir:         filepath.Join(dir, "certs"),
		ServerHosts: []string{"127.0.0.1", "localhost"},
		ClientIDs:   []string{"sandbox-1"},
	})
	if err != nil {
		t.Fatalf("PrepareCertificates() error = %v", err)
	}

	dbPath := filepath.Join(dir, "audit.db")
	worker, err := NewServer(ctx, Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   dbPath,
		Recording: RecordingConfig{
			Enabled:   true,
			QueueSize: 16,
		},
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	workerErrCh := make(chan error, 1)
	go func() {
		workerErrCh <- worker.ListenAndServe()
	}()
	closeWorker := closeProxyServer(t, worker, workerErrCh)
	t.Cleanup(closeWorker)
	workerAddr := waitForAddr(t, worker)

	clientMaterial := prepared.Clients["sandbox-1"]
	local, err := bridge.New(ctx, bridge.Config{
		ListenAddress:  "127.0.0.1:0",
		WorkerProxyURL: "https://" + workerAddr.String(),
		MTLSCAPath:     clientMaterial.MTLSCAPath,
		ClientCertPath: clientMaterial.ClientCertPath,
		ClientKeyPath:  clientMaterial.ClientKeyPath,
	})
	if err != nil {
		t.Fatalf("bridge.New() error = %v", err)
	}
	localErrCh := make(chan error, 1)
	go func() {
		localErrCh <- local.ListenAndServe()
	}()
	closeLocal := closeLocalForwarder(t, local, localErrCh)
	t.Cleanup(closeLocal)
	localAddr := waitForLocalForwarderAddr(t, local)

	proxyURL, err := url.Parse("http://" + localAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Get() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	closeLocal()
	closeWorker()

	pools, err := gormdb.Open(gormdb.Config{DSN: dbPath})
	if err != nil {
		t.Fatalf("open audit db: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	var exchange audit.HTTPExchange
	if err := pools.Read.Where("client_id = ?", "sandbox-1").First(&exchange).Error; err != nil {
		t.Fatalf("read audit exchange: %v", err)
	}
	if exchange.Status != http.StatusOK {
		t.Fatalf("audit status = %d", exchange.Status)
	}
}

// portProbeUserAgent is how the sandbox-agent's port probe names itself
// (`sandbox-agent/ports/probe.go`).
const portProbeUserAgent = "discobox-sandbox-agent (port probe)"

// TestHTTPProxyUpgradeEarlyClientBytes pins the bytes a client sends in the
// same write as its upgrade request.
//
// net/http parses the request with a bufio.Reader, so anything that arrives in
// the same segment is already in that buffer when the handler runs. goproxy
// hijacks the connection and throws that reader away (hijackConnection in
// websocket.go), then relays from the raw connection — so without the wrapper
// this proxy puts in front of it, those bytes reach nobody: the origin waits
// for a request it will never see and the client waits for an answer to it.
//
// TestHTTPProxyUpgradeAudit exercises the same path but writes after reading
// the 101, so it only fails when the scheduler puts the client's write first.
// That made it a ten-minute CI hang roughly one run in two and it passes every
// time locally. This one sends both in one write, so it fails every time.
func TestHTTPProxyUpgradeEarlyClientBytes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	originErrCh := make(chan error, 1)
	origin := newOrigin(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = rw.Flush()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(rw, buf); err != nil {
			originErrCh <- err
			return
		}
		originErrCh <- nil
		_, _ = rw.WriteString("pong")
		_ = rw.Flush()
	})
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	prepared, err := PrepareCertificates(PrepareOptions{
		Dir:         filepath.Join(dir, "certs"),
		ServerHosts: []string{"127.0.0.1", "localhost"},
		ClientIDs:   []string{"sandbox-1"},
	})
	if err != nil {
		t.Fatalf("PrepareCertificates() error = %v", err)
	}
	server, err := NewServer(ctx, Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   filepath.Join(dir, "audit.db"),
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()
	t.Cleanup(closeProxyServer(t, server, errCh))
	addr := waitForAddr(t, server)

	conn := dialProxyMTLS(ctx, t, addr.String(), prepared.Clients["sandbox-1"])
	// A deadline rather than the package timeout: the failure this pins is a
	// hang, and a hang that waits for `go test` to give up costs ten minutes
	// and reports a panic rather than this test's name.
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	// The whole point: one write carrying the request and the first payload.
	request := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", origin.URL, originURL.Host)
	if _, err := conn.Write(append([]byte(request), []byte("ping")...)); err != nil {
		t.Fatalf("write upgrade request and payload: %v", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}

	got := make([]byte, 4)
	if _, err := io.ReadFull(reader, got); err != nil {
		select {
		case originErr := <-originErrCh:
			t.Fatalf("read reply: %v; the origin's own read: %v", err, originErr)
		default:
		}
		t.Fatalf("read reply: %v (the origin never saw the payload sent with the request)", err)
	}
	if string(got) != "pong" {
		t.Fatalf("reply = %q, want \"pong\"", got)
	}
}

// ignoringPortProbe answers the sandbox-agent's port probe itself rather than
// passing it to the handler: every port that starts listening on the machine is
// asked `HEAD /` once, and this repository is worked on inside a sandbox, so a
// test that counts or records what reached its upstream would otherwise be
// counting a request the proxy never sent.
func ignoringPortProbe(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == portProbeUserAgent {
			return
		}
		handler(w, r)
	}
}

// newOrigin is an upstream for a test to point the proxy at.
func newOrigin(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(ignoringPortProbe(handler))
}

// newTLSOrigin is the same for an upstream the proxy has to MITM. HTTP/2 is off
// so these tests exercise the HTTP/1.1 leg; grpc_integration_test.go covers h2.
func newTLSOrigin(handler http.HandlerFunc) *httptest.Server {
	origin := httptest.NewUnstartedServer(ignoringPortProbe(handler))
	origin.EnableHTTP2 = false
	origin.StartTLS()
	return origin
}

func waitForAddr(t *testing.T, server *Server) net.Addr {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr := server.Addr(); addr != nil {
			return addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for proxy address")
	return nil
}

func waitForLocalForwarderAddr(t *testing.T, forwarder *bridge.Forwarder) net.Addr {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr := forwarder.Addr(); addr != nil {
			return addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for local forwarder address")
	return nil
}

func waitForHTTPExchange(t *testing.T, dsn, query string, args ...any) audit.HTTPExchange {
	t.Helper()
	pools, err := gormdb.Open(gormdb.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open audit db: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	defer func() {
		if err := pools.Close(); err != nil {
			t.Errorf("close audit db: %v", err)
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var exchange audit.HTTPExchange
		result := pools.Read.Where(query, args...).Limit(1).Find(&exchange)
		if result.Error != nil {
			t.Fatalf("read audit exchange: %v", result.Error)
		}
		if result.RowsAffected > 0 {
			return exchange
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for audit exchange")
	return audit.HTTPExchange{}
}

// waitForStreamSpool reads the spool until it holds every payload. The exchange
// row and the spool file are finished by different goroutines and the row lands
// first, so a read taken as soon as the row appears can catch the file
// mid-flush — which fails as a spool that is simply missing the bytes.
func waitForStreamSpool(t *testing.T, path string, payloads ...[]byte) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last []byte
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read stream spool: %v", err)
		}
		if err == nil {
			last = data
			recorded := streamSpoolPayloads(data)
			complete := true
			for _, payload := range payloads {
				if !streamSpoolHasPayload(recorded, payload) {
					complete = false
					break
				}
			}
			if complete {
				return data
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the stream spool to hold the upgraded payloads; got %q", last)
	return nil
}

func closeProxyServer(t *testing.T, server *Server, errCh <-chan error) func() {
	t.Helper()
	var closeOnce sync.Once
	return func() {
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
}

func closeLocalForwarder(t *testing.T, forwarder *bridge.Forwarder, errCh <-chan error) func() {
	t.Helper()
	var closeOnce sync.Once
	return func() {
		closeOnce.Do(func() {
			if err := forwarder.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			select {
			case err := <-errCh:
				if err != nil {
					t.Fatalf("ListenAndServe() error = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for local forwarder shutdown")
			}
		})
	}
}

func mtlsHTTPClient(t *testing.T, addr string, material ClientMaterial) *http.Client {
	t.Helper()
	clientCert, err := tls.LoadX509KeyPair(material.ClientCertPath, material.ClientKeyPath)
	if err != nil {
		t.Fatalf("load client certificate: %v", err)
	}
	caBytes, err := os.ReadFile(material.MTLSCAPath)
	if err != nil {
		t.Fatalf("read mTLS CA: %v", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caBytes) {
		t.Fatal("failed to parse mTLS CA")
	}
	proxyURL, err := url.Parse("https://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			RootCAs:      caPool,
			Certificates: []tls.Certificate{clientCert},
			MinVersion:   tls.VersionTLS12,
		},
	}}
}

func assertSpoolFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read spool file %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("spool file %s = %q, want %q", path, got, want)
	}
}

// streamSpoolPayloads reassembles what a spool file recorded, one stream per
// direction.
//
// A frame is not a payload boundary: a chunk is whatever one read returned, so
// four bytes written together arrive as "p" and "ing" when part of them was
// already in the request parser's buffer and the rest was still on the wire.
// Searching the file for the bytes as written would make the test depend on
// that split, which nothing promises.
//
// The format is the one streams.go writes: a header of the magic, a version
// byte, a timestamp, and two length-prefixed strings, then frames of a type
// byte, a timestamp, a direction, a length and the payload. Every integer is
// big-endian. A short read returns what has been parsed so far, since the
// caller polls a file that is still being written.
func streamSpoolPayloads(data []byte) map[byte][]byte {
	recorded := map[byte][]byte{}
	read := func(n int) []byte {
		if len(data) < n {
			data = nil
			return nil
		}
		taken := data[:n]
		data = data[n:]
		return taken
	}
	if magic := read(4); string(magic) != "DBS1" {
		return recorded
	}
	read(1) // version
	read(8) // started at
	if length := read(2); length != nil {
		read(int(binary.BigEndian.Uint16(length))) // session id
	}
	if length := read(2); length != nil {
		read(int(binary.BigEndian.Uint16(length))) // upgrade type
	}
	for len(data) > 0 {
		frameType := read(1)
		if frameType == nil {
			break
		}
		if frameType[0] != 1 { // a summary frame, which ends the file
			break
		}
		read(8) // timestamp
		direction := read(1)
		length := read(4)
		if direction == nil || length == nil {
			break
		}
		payload := read(int(binary.BigEndian.Uint32(length)))
		if payload == nil {
			break
		}
		recorded[direction[0]] = append(recorded[direction[0]], payload...)
	}
	return recorded
}

func streamSpoolHasPayload(recorded map[byte][]byte, payload []byte) bool {
	for _, stream := range recorded {
		if bytes.Contains(stream, payload) {
			return true
		}
	}
	return false
}

func dialProxyMTLS(ctx context.Context, t *testing.T, addr string, material ClientMaterial) net.Conn {
	t.Helper()
	clientCert, err := tls.LoadX509KeyPair(material.ClientCertPath, material.ClientKeyPath)
	if err != nil {
		t.Fatalf("load client certificate: %v", err)
	}
	caBytes, err := os.ReadFile(material.MTLSCAPath)
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
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	return conn
}

func TestHTTPProxySecretSwapOverMITM(t *testing.T) {
	// The assertion needs the request to reach the handler, which writes the
	// audit row, even when goproxy's verifying MITM transport then rejects the
	// self-signed test origin (see the comment on client.Do below). On Windows
	// the MITM leg fails earlier than that, so no row is written and there is
	// nothing to assert against. The proxy itself only ever runs on Linux.
	if runtime.GOOS == "windows" {
		t.Skip("goproxy's MITM leg fails before the handler runs on Windows")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-proj-SENTINEL0000000000000000000000000000000000"
	const realValue = "sk-proj-REALVALUE1111111111111111111111111111111111"

	var sawAuth string
	origin := newTLSOrigin(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	})
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	originHost := originURL.Hostname()

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
	server, err := NewServer(ctx, Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   filepath.Join(dir, "audit.db"),
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16},
		Secrets: SecretsConfig{
			Clients: []SecretClient{{ClientID: "sandbox-1", Sentinels: []string{sentinel}}},
		},
	}, prepared.Bundle, stubResolver{value: realValue, host: originHost})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	// The self-signed test origin would be rejected by the proxy's verifying
	// upstream transport; trust it so the round-trip completes and the swap is
	// observable at the origin and in the audit trail.
	server.http.proxy.Tr = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test origin is self-signed
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	t.Cleanup(func() { _ = server.Close(); <-errCh })
	addr := waitForAddr(t, server)

	// Client trusts both the mTLS CA (proxy) and the MITM CA (intercepted origin),
	// and presents the sandbox client certificate.
	material := prepared.Clients["sandbox-1"]
	clientCert, err := tls.LoadX509KeyPair(material.ClientCertPath, material.ClientKeyPath)
	if err != nil {
		t.Fatalf("load client cert: %v", err)
	}
	pool := x509.NewCertPool()
	for _, p := range []string{material.MTLSCAPath, material.MITMCAPath} {
		pem, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read CA %s: %v", p, err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			t.Fatalf("parse CA %s", p)
		}
	}
	proxyURL, _ := url.Parse("https://" + addr.String())
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{clientCert},
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{"http/1.1"},
		},
	}}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sentinel)
	// The swap runs in the request handler before the upstream send, so it is
	// verified through the audit trail even if goproxy's verifying MITM transport
	// rejects the self-signed test origin. If the request does complete, the
	// origin must have received the swapped real value.
	if resp, doErr := client.Do(req); doErr == nil {
		_ = resp.Body.Close()
		if sawAuth != "Bearer "+realValue {
			t.Fatalf("origin saw %q, want swapped real value", sawAuth)
		}
	}

	// The exchange is recorded once goproxy has finished copying the response
	// body, which can be after the client has read it; wait for the row rather
	// than closing the server on a request still being finished.
	exchange := waitForHTTPExchange(t, filepath.Join(dir, "audit.db"), "method = ?", http.MethodGet)
	if exchange.ClientID != "sandbox-1" {
		t.Fatalf("MITM'd request client_id = %q, want sandbox-1 (identity must reach MITM'd requests)", exchange.ClientID)
	}
	var headers map[string][]string
	if err := json.Unmarshal([]byte(exchange.RequestHeaders), &headers); err != nil {
		t.Fatalf("unmarshal request headers: %v", err)
	}
	if v := headers["Authorization"]; len(v) != 1 || v[0] != "[REDACTED]" {
		t.Fatalf("Authorization audit = %#v, want [REDACTED] (swap engaged)", v)
	}
	if strings.Contains(exchange.RequestHeaders, sentinel) || strings.Contains(exchange.RequestHeaders, realValue) {
		t.Fatalf("audit leaked secret material: %s", exchange.RequestHeaders)
	}
}

// Git sends a credential as `Authorization: Basic base64(user:password)`, so a
// sentinel traveling that way is invisible to a literal scan. The upstream
// must receive the real credential inside the re-encoded blob, and the audit
// must hold neither the real value nor its encoding.
func TestHTTPProxySecretSentinelSwapInBasicAuth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "ghp_SENTINELVALUE0000000000000000000000"
	const realValue = "ghp_REALSECRETVALUE1234567890abcdefghij"
	basic := func(password string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+password))
	}

	var sawAuthorization string
	origin := newOrigin(func(w http.ResponseWriter, r *http.Request) {
		sawAuthorization = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	})
	defer origin.Close()

	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}

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
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16},
		Secrets: SecretsConfig{
			Clients: []SecretClient{{ClientID: "sandbox-1", Sentinels: []string{sentinel}}},
		},
	}, prepared.Bundle, stubResolver{value: realValue, host: originURL.Hostname()})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
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

	client := mtlsHTTPClient(t, addr.String(), prepared.Clients["sandbox-1"])
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", basic(sentinel))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if sawAuthorization != basic(realValue) {
		t.Fatalf("upstream Authorization = %q, want %q", sawAuthorization, basic(realValue))
	}

	closeServer()

	pools, err := gormdb.Open(gormdb.Config{DSN: dbPath})
	if err != nil {
		t.Fatalf("open audit db: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	var exchange audit.HTTPExchange
	if err := pools.Read.Where("client_id = ?", "sandbox-1").First(&exchange).Error; err != nil {
		t.Fatalf("read audit exchange: %v", err)
	}
	if strings.Contains(exchange.RequestHeaders, realValue) {
		t.Fatalf("audit leaked real secret value: %s", exchange.RequestHeaders)
	}
	if strings.Contains(exchange.RequestHeaders, basic(realValue)) {
		t.Fatalf("audit leaked encoded secret value: %s", exchange.RequestHeaders)
	}
	var headers map[string][]string
	if err := json.Unmarshal([]byte(exchange.RequestHeaders), &headers); err != nil {
		t.Fatalf("unmarshal request headers: %v", err)
	}
	if values := headers["Authorization"]; len(values) != 1 || values[0] != "[REDACTED]" {
		t.Fatalf("Authorization audit values = %#v, want [REDACTED]", values)
	}
}

// A response that cannot carry a body — a 304, a 204, the answer to a HEAD —
// must reach the client with nothing after its headers. Anything written there
// stays on the kept-alive connection and is read as the start of the next
// response, which is how a warm cargo cache broke: every revalidation came back
// 304, and the request after it failed with "Invalid status line".
func TestHTTPProxyBodilessResponsesKeepTheConnectionInSync(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("goproxy's MITM leg fails before the handler runs on Windows")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	origin := newTLSOrigin(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/not-modified":
			w.Header().Set("ETag", `"v1"`)
			w.WriteHeader(http.StatusNotModified)
		case "/no-content":
			w.WriteHeader(http.StatusNoContent)
		case "/not-modified-chunked", "/no-content-chunked":
			// RFC 9110 lets a 304 carry the Transfer-Encoding its 200 would
			// have, still with no body; net/http will not write one, so the
			// head goes out by hand.
			status := "304 Not Modified"
			if r.URL.Path == "/no-content-chunked" {
				status = "204 No Content"
			}
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 " + status + "\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n")
			_ = rw.Flush()
		case "/head-chunked":
			// The GET this HEAD describes would be chunked; the HEAD says so and
			// carries nothing.
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n")
			_ = rw.Flush()
		case "/head":
			w.Header().Set("Content-Length", "4")
			if r.Method != http.MethodHead {
				_, _ = io.WriteString(w, "body")
			}
		default:
			_, _ = io.WriteString(w, "done")
		}
	})
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	prepared, err := PrepareCertificates(PrepareOptions{
		Dir:         filepath.Join(dir, "certs"),
		ServerHosts: []string{"127.0.0.1", "localhost"},
		ClientIDs:   []string{"sandbox-1"},
	})
	if err != nil {
		t.Fatalf("PrepareCertificates() error = %v", err)
	}
	server, err := NewServer(ctx, Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   filepath.Join(dir, "audit.db"),
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16},
		Cache: CacheConfig{
			Enabled:      true,
			Dir:          filepath.Join(dir, "cache"),
			MaxSizeBytes: 1024 * 1024,
			Patterns:     []string{"/cached-no-content"},
		},
	}, prepared.Bundle, nil)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	server.http.proxy.Tr = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test origin is self-signed
	// A 204 stored before bodiless responses stopped being cached: the cache
	// outlives an upgrade, so a hit on one must be relayed without a body too.
	put, err := server.http.cache.BeginStreamingPut(originURL.Host+"/cached-no-content", &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}})
	if err != nil {
		t.Fatalf("seed cached 204: %v", err)
	}
	if err := put.Commit(); err != nil {
		t.Fatalf("commit cached 204: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	t.Cleanup(closeProxyServer(t, server, errCh))
	addr := waitForAddr(t, server)

	material := prepared.Clients["sandbox-1"]
	conn := dialProxyMTLS(ctx, t, addr.String(), material)
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", originURL.Host, originURL.Host); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	connectReader := bufio.NewReader(conn)
	connectResp, err := http.ReadResponse(connectReader, nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	_ = connectResp.Body.Close()
	if connectResp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", connectResp.StatusCode)
	}

	mitmCA, err := os.ReadFile(material.MITMCAPath)
	if err != nil {
		t.Fatalf("read MITM CA: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(mitmCA) {
		t.Fatal("parse MITM CA")
	}
	tunnel := tls.Client(conn, &tls.Config{
		RootCAs:    roots,
		ServerName: originURL.Hostname(),
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	})
	reader := bufio.NewReader(tunnel)

	exchange := func(method, path, extra string) (int, http.Header, bool) {
		t.Helper()
		if _, err := fmt.Fprintf(tunnel, "%s %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", method, path, originURL.Host, extra); err != nil {
			t.Fatalf("%s %s: write: %v", method, path, err)
		}
		req := &http.Request{Method: method}
		resp, err := http.ReadResponse(reader, req)
		if err != nil {
			t.Fatalf("%s %s: read response: %v (the previous response left bytes on the connection)", method, path, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("%s %s: read body: %v", method, path, err)
		}
		if len(body) > 0 && (method == http.MethodHead || resp.StatusCode == http.StatusNotModified || resp.StatusCode == http.StatusNoContent) {
			t.Fatalf("%s %s: body = %q, want none", method, path, body)
		}
		return resp.StatusCode, resp.Header, resp.Close
	}

	if status, _, _ := exchange(http.MethodGet, "/not-modified", "If-None-Match: \"v1\"\r\n"); status != http.StatusNotModified {
		t.Fatalf("GET /not-modified status = %d, want 304", status)
	}
	if status, _, _ := exchange(http.MethodGet, "/no-content", ""); status != http.StatusNoContent {
		t.Fatalf("GET /no-content status = %d, want 204", status)
	}
	if status, _, _ := exchange(http.MethodGet, "/not-modified-chunked", ""); status != http.StatusNotModified {
		t.Fatalf("GET /not-modified-chunked status = %d, want 304", status)
	}
	if status, _, _ := exchange(http.MethodGet, "/no-content-chunked", ""); status != http.StatusNoContent {
		t.Fatalf("GET /no-content-chunked status = %d, want 204", status)
	}
	if status, _, _ := exchange(http.MethodGet, "/cached-no-content", ""); status != http.StatusNoContent {
		t.Fatalf("GET /cached-no-content status = %d, want the cached 204", status)
	}
	if _, _, closing := exchange(http.MethodHead, "/head-chunked", ""); closing {
		t.Fatal("HEAD /head-chunked closed the connection; a HEAD's transfer coding describes its GET and must pass through")
	}
	if _, header, _ := exchange(http.MethodHead, "/head", ""); header.Get("Content-Length") != "4" {
		t.Fatalf("HEAD /head Content-Length = %q, want 4 (the length of the GET it describes)", header.Get("Content-Length"))
	}

	// The request that broke: the next one on the same connection.
	if _, err := fmt.Fprintf(tunnel, "GET /after HTTP/1.1\r\nHost: %s\r\n\r\n", originURL.Host); err != nil {
		t.Fatalf("GET /after: write: %v", err)
	}
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("GET /after: read response: %v (a bodiless response before it left bytes on the connection)", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("GET /after: read body: %v", err)
	}
	if string(body) != "done" {
		t.Fatalf("GET /after body = %q, want \"done\"", body)
	}

	// The HEAD relayed no body, whatever length it announced.
	head := waitForHTTPExchange(t, filepath.Join(dir, "audit.db"), "method = ?", http.MethodHead)
	if head.ResponseBytes != 0 {
		t.Fatalf("HEAD audit ResponseBytes = %d, want 0", head.ResponseBytes)
	}
}

// tunnelOrigin records what the origin behind a tunnel saw.
type tunnelOrigin struct {
	mu   sync.Mutex
	seen []tunnelOriginRequest
}

type tunnelOriginRequest struct {
	method string
	host   string
	path   string
	tls    bool
	auth   string
	body   string
}

func (o *tunnelOrigin) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	o.mu.Lock()
	o.seen = append(o.seen, tunnelOriginRequest{
		method: r.Method, host: r.Host, path: r.URL.Path, tls: r.TLS != nil,
		auth: r.Header.Get("Authorization"), body: string(body),
	})
	o.mu.Unlock()
	_, _ = io.WriteString(w, "ok "+r.URL.Path)
}

func (o *tunnelOrigin) requests() []tunnelOriginRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]tunnelOriginRequest(nil), o.seen...)
}

// startTunnelProxy starts a proxy whose upstream transport sends every dial
// to origin, so a CONNECT to any name and port reaches it. It returns the
// proxy's address and the sandbox's client material.
func startTunnelProxy(ctx context.Context, t *testing.T, origin *url.URL, resolver stubResolver, sentinel string) (net.Addr, ClientMaterial) {
	t.Helper()
	dir := t.TempDir()
	prepared, err := PrepareCertificates(PrepareOptions{
		Dir:         filepath.Join(dir, "certs"),
		ServerHosts: []string{"127.0.0.1", "localhost"},
		ClientIDs:   []string{"sandbox-1"},
	})
	if err != nil {
		t.Fatalf("PrepareCertificates() error = %v", err)
	}
	cfg := Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   filepath.Join(dir, "audit.db"),
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16},
	}
	if sentinel != "" {
		cfg.Secrets = SecretsConfig{Clients: []SecretClient{{ClientID: "sandbox-1", Sentinels: []string{sentinel}}}}
	}
	server, err := NewServer(ctx, cfg, prepared.Bundle, resolver)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	var dialer net.Dialer
	server.http.proxy.Tr = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, origin.Host)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test origin is self-signed
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	t.Cleanup(closeProxyServer(t, server, errCh))
	return waitForAddr(t, server), prepared.Clients["sandbox-1"]
}

// openTunnel sends CONNECT authority over mTLS and returns the tunnel once the
// proxy has answered 200.
func openTunnel(ctx context.Context, t *testing.T, addr net.Addr, material ClientMaterial, authority string) net.Conn {
	t.Helper()
	conn := dialProxyMTLS(ctx, t, addr.String(), material)
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: zig/0.15.2 (std.http)\r\n\r\n", authority, authority); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	// The proxy's 200 is a bare status line; read it byte-exact so nothing of
	// the tunnel is buffered away.
	head := make([]byte, len("HTTP/1.0 200 OK\r\n\r\n"))
	if _, err := io.ReadFull(conn, head); err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if !strings.HasPrefix(string(head), "HTTP/1.0 200") {
		t.Fatalf("CONNECT response = %q, want 200", head)
	}
	return conn
}

type tunnelExchange struct {
	conn   io.Writer
	reader *bufio.Reader
}

func (x tunnelExchange) do(t *testing.T, method, raw string) (int, string) {
	t.Helper()
	if _, err := io.WriteString(x.conn, raw); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(x.reader, &http.Request{Method: method})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

// Zig's HTTP client opens a CONNECT tunnel to :443 and then speaks plaintext
// HTTP inside it. The proxy answers it in plaintext and speaks TLS to the
// origin, for every request on the kept-alive connection, a chunked upload
// included (git smart-HTTP's POST /git-upload-pack).
func TestTunnelUpgradesPlaintextHTTPTo443(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("goproxy's MITM leg fails before the handler runs on Windows")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var origin tunnelOrigin
	server := newTLSOrigin(origin.handler)
	defer server.Close()
	originURL, _ := url.Parse(server.URL)
	addr, material := startTunnelProxy(ctx, t, originURL, stubResolver{}, "")

	conn := openTunnel(ctx, t, addr, material, "deps.example.test:443")
	x := tunnelExchange{conn: conn, reader: bufio.NewReader(conn)}

	if status, body := x.do(t, http.MethodGet, "GET /pkg.tar.gz HTTP/1.1\r\nHost: deps.example.test\r\nUser-Agent: zig/0.15.2 (std.http)\r\n\r\n"); status != http.StatusOK || body != "ok /pkg.tar.gz" {
		t.Fatalf("tunneled plaintext GET to :443 = %d %q, want 200 \"ok /pkg.tar.gz\"", status, body)
	}
	if status, body := x.do(t, http.MethodGet, "GET /info/refs HTTP/1.1\r\nHost: deps.example.test:443\r\n\r\n"); status != http.StatusOK || body != "ok /info/refs" {
		t.Fatalf("second GET on the kept-alive tunnel = %d %q, want 200", status, body)
	}
	if status, body := x.do(t, http.MethodPost, "POST /git-upload-pack HTTP/1.1\r\nHost: deps.example.test\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n"); status != http.StatusOK || body != "ok /git-upload-pack" {
		t.Fatalf("chunked POST on the kept-alive tunnel = %d %q, want 200", status, body)
	}

	seen := origin.requests()
	if len(seen) != 3 {
		t.Fatalf("origin saw %d requests, want 3: %+v", len(seen), seen)
	}
	for _, r := range seen {
		if !r.tls {
			t.Fatalf("origin saw %s %s in plaintext, want TLS", r.method, r.path)
		}
	}
	if seen[2].body != "hello world" {
		t.Fatalf("origin saw POST body %q, want \"hello world\"", seen[2].body)
	}
}

// A plaintext tunnel to any port but 443 is plaintext HTTP end to end.
func TestTunnelKeepsPlaintextHTTPToOtherPorts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("goproxy's MITM leg fails before the handler runs on Windows")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var origin tunnelOrigin
	server := newOrigin(origin.handler)
	defer server.Close()
	originURL, _ := url.Parse(server.URL)
	addr, material := startTunnelProxy(ctx, t, originURL, stubResolver{}, "")

	conn := openTunnel(ctx, t, addr, material, "plain.example.test:8080")
	x := tunnelExchange{conn: conn, reader: bufio.NewReader(conn)}
	if status, _ := x.do(t, http.MethodGet, "GET /x HTTP/1.1\r\nHost: plain.example.test:8080\r\n\r\n"); status != http.StatusOK {
		t.Fatalf("tunneled plaintext GET to :8080 = %d, want 200", status)
	}
	if seen := origin.requests(); len(seen) != 1 || seen[0].tls {
		t.Fatalf("origin saw %+v, want one plaintext request", seen)
	}
}

// A request line that names no URL — "PRI *", an HTTP/2 preface the proxy did
// not take for one — is refused, and the proxy goes on serving: it once
// dereferenced the missing URL in a goroutine with no recover, which took the
// whole pool proxy down with it.
func TestTunnelRefusesARequestWithNoURL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("goproxy's MITM leg fails before the handler runs on Windows")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var origin tunnelOrigin
	server := newOrigin(origin.handler)
	defer server.Close()
	originURL, _ := url.Parse(server.URL)
	addr, material := startTunnelProxy(ctx, t, originURL, stubResolver{}, "")

	conn := openTunnel(ctx, t, addr, material, "plain.example.test:8080")
	x := tunnelExchange{conn: conn, reader: bufio.NewReader(conn)}
	if status, _ := x.do(t, "PRI", "PRI * HTTP/2.0\r\n\r\nnot the rest of a preface\r\n\r\n"); status != http.StatusBadRequest {
		t.Fatalf("PRI * in a plaintext tunnel = %d, want 400", status)
	}

	conn = openTunnel(ctx, t, addr, material, "plain.example.test:8080")
	x = tunnelExchange{conn: conn, reader: bufio.NewReader(conn)}
	if status, _ := x.do(t, http.MethodGet, "GET /x HTTP/1.1\r\nHost: plain.example.test:8080\r\n\r\n"); status != http.StatusOK {
		t.Fatalf("GET after the refused request = %d, want 200", status)
	}
	if seen := origin.requests(); len(seen) != 1 || seen[0].path != "/x" {
		t.Fatalf("origin saw %+v, want only GET /x", seen)
	}
}

// The destination of a tunneled request is the CONNECT authority, so that is
// the host policy must judge it by. A Host header naming another host would
// have the proxy swap in a credential bound to that host and send it to the
// tunnel's. The proxy refuses it, whether the tunnel is TLS or plaintext.
func TestTunnelRefusesAHostOtherThanTheConnectAuthority(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("goproxy's MITM leg fails before the handler runs on Windows")
	}
	const sentinel = "ghp_SENTINELVALUE0000000000000000000000"
	const realValue = "ghp_REALVALUE11111111111111111111111111"

	for _, tc := range []struct {
		name      string
		authority string
		wrap      func(t *testing.T, conn net.Conn, material ClientMaterial) net.Conn
	}{
		{name: "tls", authority: "attacker.example.test:443", wrap: mitmTunnel("attacker.example.test")},
		{name: "plaintext", authority: "attacker.example.test:443", wrap: func(_ *testing.T, conn net.Conn, _ ClientMaterial) net.Conn { return conn }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var origin tunnelOrigin
			server := newTLSOrigin(origin.handler)
			defer server.Close()
			originURL, _ := url.Parse(server.URL)
			addr, material := startTunnelProxy(ctx, t, originURL, stubResolver{value: realValue, host: "github.example.test"}, sentinel)

			conn := tc.wrap(t, openTunnel(ctx, t, addr, material, tc.authority), material)
			x := tunnelExchange{conn: conn, reader: bufio.NewReader(conn)}
			status, _ := x.do(t, http.MethodGet, "GET /user HTTP/1.1\r\nHost: github.example.test\r\nAuthorization: Bearer "+sentinel+"\r\n\r\n")
			for _, r := range origin.requests() {
				if strings.Contains(r.auth, realValue) {
					t.Fatalf("origin behind %s received the credential bound to github.example.test", tc.authority)
				}
			}
			if status != http.StatusMisdirectedRequest {
				t.Fatalf("tunnel to %s with Host github.example.test = %d, want 421", tc.authority, status)
			}
		})
	}
}

func mitmTunnel(serverName string) func(t *testing.T, conn net.Conn, material ClientMaterial) net.Conn {
	return func(t *testing.T, conn net.Conn, material ClientMaterial) net.Conn {
		t.Helper()
		mitmCA, err := os.ReadFile(material.MITMCAPath)
		if err != nil {
			t.Fatalf("read MITM CA: %v", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(mitmCA) {
			t.Fatal("parse MITM CA")
		}
		return tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: serverName, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}})
	}
}

// A tunneled request names the tunnel's host however its Host spells it: in
// another case, with a trailing dot, with or without the port, or an IPv6
// literal with its brackets and no port, as clients send for the default port.
func TestConnectTunnelMatchesHostsAsPolicyDoes(t *testing.T) {
	for _, tc := range []struct {
		authority, host string
		misdirected     bool
	}{
		{authority: "deps.example.test:443", host: "deps.example.test"},
		{authority: "deps.example.test:443", host: "Deps.Example.Test.:443"},
		{authority: "deps.example.test:8443", host: "deps.example.test:8443"},
		{authority: "[2001:db8::1]:443", host: "[2001:db8::1]"},
		{authority: "[2001:db8::1]:443", host: "[2001:DB8::1]:443"},
		{authority: "deps.example.test:443", host: "github.example.test", misdirected: true},
		{authority: "[2001:db8::1]:443", host: "[2001:db8::2]", misdirected: true},
	} {
		tunnel := &connectTunnel{authority: tc.authority}
		req := &http.Request{Host: tc.host, URL: &url.URL{Scheme: "https", Host: tc.authority, Path: "/"}}
		if got := tunnel.misdirected(req); got != tc.misdirected {
			t.Errorf("tunnel %s, Host %q: misdirected = %v, want %v", tc.authority, tc.host, got, tc.misdirected)
		}
	}
}

// Only a plaintext request for port 443 is upgraded: an absolute-form request
// in a :443 tunnel goes to the tunnel's port unless it names another, and one
// that names another port stays plaintext.
func TestConnectTunnelUpgradesOnlyPort443(t *testing.T) {
	tunnel := &connectTunnel{authority: "deps.example.test:443"}
	for _, tc := range []struct {
		url      string
		upgraded bool
	}{
		{url: "http://deps.example.test:443/pkg", upgraded: true},
		{url: "http://deps.example.test/pkg", upgraded: true},
		{url: "http://deps.example.test:80/pkg"},
		{url: "https://deps.example.test:443/pkg"},
	} {
		u, err := url.Parse(tc.url)
		if err != nil {
			t.Fatal(err)
		}
		req := &http.Request{URL: u}
		if got := tunnel.upgrade(req); got != tc.upgraded {
			t.Errorf("%s: upgraded = %v, want %v", tc.url, got, tc.upgraded)
		}
	}
}
