package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/gormdb"
	"github.com/discobox-ai/discobox/proxy/bridge"
	"github.com/discobox-ai/discobox/proxy/internal/audit"
	"github.com/discobox-ai/discobox/proxy/internal/secrets"
)

type stubResolver struct {
	value string
	host  string
}

func (r stubResolver) Resolve(_ context.Context, req secrets.ResolveRequest) (secrets.ResolveResult, error) {
	if r.host != "" && req.Host != r.host {
		return secrets.ResolveResult{}, secrets.ErrDenied
	}
	return secrets.ResolveResult{Value: r.value, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func TestHTTPProxyMTLSIdentityHeaderRewriteAndAudit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var sawAuthorization string
	var sawInjectedSecret string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthorization = r.Header.Get("Authorization")
		sawInjectedSecret = r.Header.Get("X-Injected-Secret")
		_, _ = io.WriteString(w, "ok")
	}))
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

func TestHTTPProxySecretSentinelSwapAndAudit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"
	const realValue = "sk-ant-oat01-REALSECRETVALUE1234567890abcdefgh"

	var sawAuthorization string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthorization = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	}))
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
	}, prepared.Bundle, stubResolver{value: realValue, host: originHost})
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
}

func TestHTTPProxySecretSentinelDeniedForOtherHost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"
	const realValue = "sk-ant-oat01-REALSECRETVALUE1234567890abcdefgh"

	var sawAuthorization string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthorization = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	}))
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
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
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
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/audit/http/"+strconv.FormatUint(uint64(exchange.ID), 10)+"/"+tc.path+"?client_id=sandbox-1", nil)
		rec := httptest.NewRecorder()
		server.ControlHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("control %s status = %d body=%q", tc.path, rec.Code, rec.Body.String())
		}
		if rec.Body.String() != tc.want {
			t.Fatalf("control %s body = %q", tc.path, rec.Body.String())
		}
	}
}

func TestHTTPProxyCapturesCachedResponseBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const responseBody = "cached response body"
	var originHits int
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		originHits++
		_, _ = io.WriteString(w, responseBody)
	}))
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

	closeServer()

	pools, err := gormdb.Open(gormdb.Config{DSN: dbPath})
	if err != nil {
		t.Fatalf("open audit db: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	var exchange audit.HTTPExchange
	if err := pools.Read.Where("client_id = ? AND cache_hit = ?", "sandbox-1", true).First(&exchange).Error; err != nil {
		t.Fatalf("read cache-hit audit exchange: %v", err)
	}
	if exchange.ResponseBodyBytes != int64(len(responseBody)) || exchange.ResponseBodyFile == "" {
		t.Fatalf("cached response body metadata bytes=%d file=%q", exchange.ResponseBodyBytes, exchange.ResponseBodyFile)
	}
	assertSpoolFile(t, filepath.Join(bodyDir, filepath.FromSlash(exchange.ResponseBodyFile)), responseBody)
}

func TestHTTPProxyUpgradeAudit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	originErrCh := make(chan error, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
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
	streamBytes, err := os.ReadFile(filepath.Join(dir, "streams", filepath.FromSlash(exchange.StreamFile)))
	if err != nil {
		t.Fatalf("read stream spool: %v", err)
	}
	if !bytes.Contains(streamBytes, []byte("ping")) || !bytes.Contains(streamBytes, []byte("pong")) {
		t.Fatal("stream spool did not contain upgraded payloads")
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/audit/http/"+strconv.FormatUint(uint64(exchange.ID), 10)+"/stream?client_id=sandbox-1", nil)
	rec := httptest.NewRecorder()
	server.ControlHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("control stream status = %d body=%q", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), streamBytes) {
		t.Fatal("control stream response did not match spool file")
	}
}

func TestLocalForwarderHTTPToWorkerProxy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
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
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	}))
	origin.EnableHTTP2 = false
	origin.StartTLS()
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
	var closeOnce sync.Once
	closeServer := func() { closeOnce.Do(func() { _ = server.Close(); <-errCh }) }
	t.Cleanup(closeServer)
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

	closeServer()

	pools, err := gormdb.Open(gormdb.Config{DSN: filepath.Join(dir, "audit.db")})
	if err != nil {
		t.Fatalf("open audit db: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	var exchange audit.HTTPExchange
	if err := pools.Read.Where("method = ?", http.MethodGet).Order("id DESC").First(&exchange).Error; err != nil {
		t.Fatalf("read audit exchange for MITM'd request: %v", err)
	}
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
