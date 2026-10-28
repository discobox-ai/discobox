package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy/internal/audit"
	"github.com/discobox-ai/discobox/proxy/internal/secrets"
	"github.com/discobox-ai/x/gormdb"
)

// routedRawResolver holds a raw binding to every path but /off-route. With
// rawDown set, it cannot be asked for a raw value at all.
type routedRawResolver struct {
	stubResolver
	rawDown bool
}

func (r routedRawResolver) ResolveRaw(ctx context.Context, req secrets.RawRequest) (secrets.RawResult, error) {
	if r.rawDown {
		return secrets.RawResult{}, errors.New("control plane unreachable")
	}
	return r.stubResolver.ResolveRaw(ctx, req)
}

func (routedRawResolver) Authorize(_ context.Context, req secrets.AuthorizeRequest) (secrets.Verdict, error) {
	if strings.Contains(req.URL, "/off-route") {
		return secrets.Verdict{Reason: "not a route of this credential"}, nil
	}
	return secrets.Verdict{Allow: true}, nil
}

// A request carrying a raw secret off its routes is refused and never sent,
// however the value is spelled or wherever it is in the request; one on its
// routes goes, recorded as spending it; and the audit trail keeps the value
// nowhere (ADR 26-10-27-840).
func TestRawSecretIsWatchedForAndKeptOutOfTheAudit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const rawSentinel = "RAW-SENTINEL-HANDLE"
	const rawValue = "AKIA-RAW-SECRET-VALUE-0123456789"

	var reached atomic.Int64
	var mu sync.Mutex
	var seen []string
	origin := newOrigin(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.URL.String()+" "+r.Header.Get("X-Api-Key")+" "+string(body))
		mu.Unlock()
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
	bodyDir := filepath.Join(dir, "bodies")
	resolver := routedRawResolver{stubResolver: stubResolver{raw: map[string]secrets.RawResult{
		rawSentinel: {Value: rawValue, SecretID: "sec_raw", BindingID: "sbd_raw"},
	}}}
	server, err := NewServer(ctx, Config{
		ListenAddress:             "127.0.0.1:0",
		AllowLoopbackDestinations: true,
		CertDir:                   prepared.Bundle.Dir,
		DatabaseDSN:               dbPath,
		Recording:                 RecordingConfig{Enabled: true, QueueSize: 64, BodyDir: bodyDir},
		Cache: CacheConfig{
			Enabled:      true,
			Dir:          filepath.Join(dir, "cache"),
			MaxSizeBytes: 1 << 20,
			Patterns:     []string{"/cached"},
		},
		Secrets: SecretsConfig{
			ScanQuery: true,
			Clients:   []SecretClient{{ClientID: "sandbox-1", Raw: []string{rawSentinel}}},
		},
	}, prepared.Bundle, resolver)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	var closeOnce sync.Once
	closeServer := func() {
		closeOnce.Do(func() {
			_ = server.Close()
			select {
			case <-errCh:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for proxy shutdown")
			}
		})
	}
	t.Cleanup(closeServer)
	addr := waitForAddr(t, server)

	handed := server.RawSecrets(ctx, "sandbox-1")
	if len(handed) != 1 || handed[0].Value != rawValue || handed[0].Sentinel != rawSentinel {
		t.Fatalf("RawSecrets() = %+v, want the binding's value", handed)
	}
	if got := server.RawSecrets(ctx, "sandbox-2"); len(got) != 0 {
		t.Fatalf("another client's raw secrets = %+v", got)
	}

	client := mtlsHTTPClient(t, addr.String(), prepared.Clients["sandbox-1"])
	send := func(method, path string, header http.Header, body io.Reader, length int64) (int, error) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, origin.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		for name, values := range header {
			req.Header[name] = values
		}
		if body != nil {
			req.ContentLength = length
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}
	status := func(code int, err error) int {
		t.Helper()
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		return code
	}

	// On its routes: sent, as the sandbox spelled it.
	if got := status(send(http.MethodGet, "/on-route?key="+url.QueryEscape(rawValue), http.Header{"X-Api-Key": {rawValue}}, nil, 0)); got != http.StatusOK {
		t.Fatalf("on-route status = %d, want 200", got)
	}
	if reached.Load() != 1 {
		t.Fatalf("origin reached %d times, want the on-route request", reached.Load())
	}

	// Never cached: the cache's key and metadata are on the pool's disk.
	for range 2 {
		if got := status(send(http.MethodGet, "/on-route/cached", http.Header{"X-Api-Key": {rawValue}}, nil, 0)); got != http.StatusOK {
			t.Fatalf("cacheable on-route status = %d, want 200", got)
		}
	}
	if reached.Load() != 3 {
		t.Fatalf("origin reached %d times, want every request carrying the value sent on", reached.Load())
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "cache")); len(entries) != 0 {
		t.Fatalf("cache holds %d entries for requests carrying a raw value", len(entries))
	}

	// Off its routes, in a header, the query, a Basic token, base64, or a body.
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:"+rawValue))
	encoded := base64.RawURLEncoding.EncodeToString([]byte(rawValue))
	body := `{"password":"` + rawValue + `"}`
	refused := []struct {
		name   string
		path   string
		header http.Header
		body   string
	}{
		{name: "header", path: "/off-route", header: http.Header{"X-Api-Key": {rawValue}}},
		{name: "query", path: "/off-route?key=" + url.QueryEscape(rawValue)},
		{name: "basic", path: "/off-route", header: http.Header{"Authorization": {basic}}},
		{name: "base64", path: "/off-route/" + encoded},
		{name: "body", path: "/off-route", body: body},
	}
	for _, tc := range refused {
		var reader io.Reader
		if tc.body != "" {
			reader = strings.NewReader(tc.body)
		}
		method := http.MethodGet
		if reader != nil {
			method = http.MethodPost
		}
		if got := status(send(method, tc.path, tc.header, reader, int64(len(tc.body)))); got != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", tc.name, got)
		}
	}

	// A streaming body is cut off before the value is whole.
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(`{"token":"` + rawValue[:10]))
		time.Sleep(50 * time.Millisecond)
		_, _ = pw.Write([]byte(rawValue[10:] + `"}`))
		_ = pw.Close()
	}()
	if code, err := send(http.MethodPost, "/off-route/stream", nil, pr, -1); err == nil && code == http.StatusOK {
		t.Errorf("streaming status = 200, want the request cut off")
	}
	mu.Lock()
	for _, got := range seen {
		if strings.Contains(got, "off-route") && strings.Contains(got, rawValue) {
			t.Errorf("the origin received the value off its routes: %q", got)
		}
	}
	mu.Unlock()

	closeServer()

	pools, err := gormdb.Open(gormdb.Config{DSN: dbPath})
	if err != nil {
		t.Fatalf("open audit db: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	var rows []audit.HTTPExchange
	if err := pools.Read.Where("client_id = ?", "sandbox-1").Find(&rows).Error; err != nil {
		t.Fatalf("read audit: %v", err)
	}
	spent, blocked, bound := 0, 0, 0
	for _, row := range rows {
		for name, text := range map[string]string{"url": row.URL, "headers": row.RequestHeaders, "reason": row.BlockedReason} {
			if strings.Contains(text, rawValue) || strings.Contains(text, encoded) {
				t.Errorf("row %d %s keeps the value: %q", row.ID, name, text)
			}
		}
		if row.SwappedSecretIDs == "sec_raw" {
			spent++
		}
		if row.RawBindingIDs == "sbd_raw" {
			bound++
		}
		if row.Blocked && strings.HasPrefix(row.BlockedReason, "raw: sbd_raw: ") {
			blocked++
		}
	}
	if spent != 3 || bound != 3 {
		t.Errorf("rows spending the raw secret = %d, naming its binding = %d, want the three on-route requests", spent, bound)
	}
	if blocked < len(refused) {
		t.Errorf("refusals naming the binding = %d, want at least %d", blocked, len(refused))
	}
	spools := os.DirFS(bodyDir)
	if err := fs.WalkDir(spools, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := fs.ReadFile(spools, path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(rawValue)) {
			t.Errorf("body spool %s keeps the value", path)
		}
		return nil
	}); err != nil {
		t.Fatalf("read body spools: %v", err)
	}
}

// A client holding a raw binding whose value the proxy could not fetch is not
// let out unwatched: its request is held through the retry window and then
// refused, never sent (ADR 26-10-27-840, fail closed).
func TestRawSecretNotYetHeldHoldsTheRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reached atomic.Int64
	origin := newOrigin(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
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
		ListenAddress:             "127.0.0.1:0",
		AllowLoopbackDestinations: true,
		CertDir:                   prepared.Bundle.Dir,
		DatabaseDSN:               filepath.Join(dir, "audit.db"),
		Secrets: SecretsConfig{
			ResolveRetrySeconds: 1,
			Clients:             []SecretClient{{ClientID: "sandbox-1", Raw: []string{"RAW-SENTINEL"}}},
		},
	}, prepared.Bundle, routedRawResolver{rawDown: true})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	go func() { _ = server.ListenAndServe() }()
	t.Cleanup(func() { _ = server.Close() })
	addr := waitForAddr(t, server)
	client := mtlsHTTPClient(t, addr.String(), prepared.Clients["sandbox-1"])
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL+"/anything", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("status = %d, Retry-After %q, want 503 with Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if reached.Load() != 0 {
		t.Fatalf("origin reached %d times, want the request never sent", reached.Load())
	}
}
