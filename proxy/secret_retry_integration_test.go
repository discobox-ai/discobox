package proxy

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy/internal/secrets"
)

// rotatingResolver hands out the next value on every resolve, so a retry that
// re-resolves gets something new — the control plane having rotated the
// credential behind the sentinel.
type rotatingResolver struct {
	mu     sync.Mutex
	values []string
	calls  int
}

func (r *rotatingResolver) Resolve(context.Context, secrets.ResolveRequest) (secrets.ResolveResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value := r.values[min(r.calls, len(r.values)-1)]
	r.calls++
	return secrets.ResolveResult{Value: value, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (r *rotatingResolver) resolveCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// startSecretProxy brings up a proxy with one sandbox and one sentinel, and
// returns an mTLS client for that sandbox.
func startSecretProxy(ctx context.Context, t *testing.T, sentinel string, resolver secrets.Resolver, tune ...func(*Config)) *http.Client {
	t.Helper()
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
	cfg := Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   filepath.Join(dir, "audit.db"),
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16},
		Secrets: SecretsConfig{
			Clients: []SecretClient{{ClientID: "sandbox-1", Sentinels: []string{sentinel}}},
		},
	}
	for _, apply := range tune {
		apply(&cfg)
	}
	server, err := NewServer(ctx, cfg, prepared.Bundle, resolver)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	t.Cleanup(func() {
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
	addr := waitForAddr(t, server)
	return mtlsHTTPClient(t, addr.String(), prepared.Clients["sandbox-1"])
}

// A sandbox holds a sentinel and cannot be at fault for a 401, so a rejected
// credential is re-resolved and the request sent again — the harness never sees
// the rejection, and so never concludes its login is gone.
func TestHTTPProxyRetriesRejectedSwappedCredential(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"
	const stale = "sk-ant-oat01-STALEVALUE0000000000000000000000"
	const rotated = "sk-ant-oat01-ROTATEDVALUE00000000000000000000"
	const body = `{"model":"claude","messages":[]}`

	var attempts atomic.Int32
	var sawBodies []string
	var mu sync.Mutex
	origin := newOrigin(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		received, _ := io.ReadAll(r.Body)
		mu.Lock()
		sawBodies = append(sawBodies, string(received))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+rotated {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"authentication_error"}`)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	defer origin.Close()

	resolver := &rotatingResolver{values: []string{stale, rotated}}
	client := startSecretProxy(ctx, t, sentinel, resolver)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sentinel)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s), want the retry's 200", resp.StatusCode, got)
	}
	if n := attempts.Load(); n != 2 {
		t.Fatalf("upstream attempts = %d, want the original and one retry", n)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, sent := range sawBodies {
		if sent != body {
			t.Fatalf("attempt %d body = %q, want the request body replayed intact", i+1, sent)
		}
	}
}

// Nothing new to send is not worth an upstream request: when re-resolving
// returns the same credential that was just rejected, the 401 is the answer.
func TestHTTPProxyDoesNotRetryWhenTheCredentialIsUnchanged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"
	const value = "sk-ant-oat01-STABLEVALUE000000000000000000000"

	var attempts atomic.Int32
	origin := newOrigin(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer origin.Close()

	resolver := &rotatingResolver{values: []string{value}}
	client := startSecretProxy(ctx, t, sentinel, resolver)

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

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the upstream 401 passed through", resp.StatusCode)
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("upstream attempts = %d, want no retry of the same credential", n)
	}
	if n := resolver.resolveCount(); n != 2 {
		t.Fatalf("resolve calls = %d, want the initial resolve plus one forced re-resolve", n)
	}
}

// A 401 that has nothing to do with a swapped credential is left alone.
func TestHTTPProxyDoesNotRetryUnswappedRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"

	var attempts atomic.Int32
	origin := newOrigin(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer origin.Close()

	client := startSecretProxy(ctx, t, sentinel, &rotatingResolver{values: []string{"unused"}})

	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, originURL.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer not-a-sentinel")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the upstream 401 passed through", resp.StatusCode)
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("upstream attempts = %d, want no retry", n)
	}
}

// settableResolver returns whatever the control plane currently holds.
type settableResolver struct {
	mu    sync.Mutex
	value string
}

func (r *settableResolver) set(value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.value = value
}

func (r *settableResolver) Resolve(context.Context, secrets.ResolveRequest) (secrets.ResolveResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return secrets.ResolveResult{Value: r.value, ExpiresAt: time.Now().Add(time.Second)}, nil
}

// The other half of a rotation: the credential the proxy picked up is the
// current one, and the upstream has not started honoring it yet. Re-resolving
// only produces the same rejected value, so the retry falls back to the one the
// rotation displaced — which is still valid, since a rotation mints the new
// credential well before the old one stops working.
func TestHTTPProxyRetriesWithTheDisplacedCredential(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-SENTINELVALUE00000000000000000000"
	const accepted = "sk-ant-oat01-ACCEPTEDVALUE0000000000000000000"
	const notYetHonoured = "sk-ant-oat01-TOOFRESHVALUE0000000000000000000"

	var attempts atomic.Int32
	origin := newOrigin(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+accepted {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	defer origin.Close()

	resolver := &settableResolver{value: accepted}
	// A one-second positive TTL is what makes the rotation observable in a
	// test: the second request re-resolves rather than serving the cache.
	client := startSecretProxy(ctx, t, sentinel, resolver, func(cfg *Config) {
		cfg.Secrets.PositiveTTLSeconds = 1
	})

	send := func() *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+sentinel)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("client.Do() error = %v", err)
		}
		return resp
	}

	first := send()
	_, _ = io.Copy(io.Discard, first.Body)
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.StatusCode)
	}

	// The control plane rotates; the upstream still only accepts the old one.
	resolver.set(notYetHonoured)
	time.Sleep(1100 * time.Millisecond)

	second := send()
	defer second.Body.Close()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want the fallback retry to succeed", second.StatusCode)
	}
	if n := attempts.Load(); n != 3 {
		t.Fatalf("upstream attempts = %d, want the first request, its rejection, and one retry", n)
	}
}
