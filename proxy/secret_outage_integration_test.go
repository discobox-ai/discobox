package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy/internal/secrets"
)

// outageResolver resolves one value until it is taken down, and then fails
// every resolve the way an unreachable control plane does: not a denial, just
// no answer.
type outageResolver struct {
	noGatewayHeaders
	reportLog
	value string
	mu    sync.Mutex
	down  bool
	calls int
}

func (r *outageResolver) Gate(context.Context, secrets.GateRequest) (secrets.GateAdmission, error) {
	return secrets.GateAdmission{}, &secrets.GateRefusal{Reason: "no gate here"}
}

func (r *outageResolver) Authorize(context.Context, secrets.AuthorizeRequest) (secrets.Verdict, error) {
	return secrets.Verdict{Allow: true}, nil
}

func (r *outageResolver) Resolve(context.Context, secrets.ResolveRequest) (secrets.ResolveResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.down {
		return secrets.ResolveResult{}, errors.New("control plane unreachable")
	}
	return secrets.ResolveResult{Value: r.value, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (r *outageResolver) takeDown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.down = true
}

func (r *outageResolver) resolveCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// sendWithAuth sends one GET through client carrying authorization, and returns
// the response's status, headers and body.
func sendWithAuth(ctx context.Context, t *testing.T, client *http.Client, url, authorization string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", authorization)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(body)
}

// A publish that changes trusts, or another sandbox's sentinels, leaves what
// this sandbox resolved in the cache, so a control plane that goes away right
// after it does not cost the sandbox its credential (ADR 26-10-14-482 §1).
func TestHTTPProxyServesACachedCredentialAcrossAPublish(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-PUBLISHSENTINEL0000000000000000000"
	const swapped = "sk-ant-oat01-PUBLISHREALVALUE000000000000000000"
	origin := newOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+swapped {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	defer origin.Close()

	resolver := &outageResolver{value: swapped}
	var cfg *Config
	server, client := startSecretProxy(ctx, t, sentinel, resolver, func(c *Config) { cfg = c })
	published := *cfg

	if status, _, _ := sendWithAuth(ctx, t, client, origin.URL, "Bearer "+sentinel); status != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", status)
	}
	resolver.takeDown()

	// Trusts only.
	published.Trusts = []HostTrust{{
		ClientID: "sandbox-2",
		Host:     "example.com:443",
		Pin:      TrustPin{Kind: TrustPinLeafSPKI, SHA256: strings.Repeat("0", 64)},
	}}
	if err := server.ApplyConfig(published); err != nil {
		t.Fatalf("ApplyConfig(trusts) error = %v", err)
	}
	// Another sandbox's sentinels.
	published.Secrets.Clients = append(published.Secrets.Clients,
		SecretClient{ClientID: "sandbox-2", Sentinels: []string{"sk-ant-oat01-SOMEONEELSE00000000000000000000"}})
	if err := server.ApplyConfig(published); err != nil {
		t.Fatalf("ApplyConfig(sentinels) error = %v", err)
	}

	if status, _, body := sendWithAuth(ctx, t, client, origin.URL, "Bearer "+sentinel); status != http.StatusOK {
		t.Fatalf("request after the publishes status = %d (%q), want 200 from the cached credential", status, body)
	}
	if calls := resolver.resolveCount(); calls != 1 {
		t.Fatalf("resolver asked %d times, want once: the second request is the cache's", calls)
	}
}

// A sentinel that cannot be resolved because the control plane is unreachable
// is never sent. The proxy waits out its retry window and then answers 503
// itself, which a client reads as an outage, where the upstream's 401 for the
// sentinel would read as a dead login (ADR 26-10-14-482 §2).
func TestHTTPProxyRefusesARequestWhoseSentinelCannotResolve(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-COLDSENTINEL000000000000000000000"
	var hits atomic.Int32
	origin := newOrigin(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer origin.Close()

	resolver := &outageResolver{value: "unused"}
	resolver.takeDown()
	var dsn string
	_, client := startSecretProxy(ctx, t, sentinel, resolver, func(cfg *Config) {
		dsn = cfg.DatabaseDSN
		cfg.Secrets.ResolveRetrySeconds = 1
	})

	start := time.Now()
	status, header, body := sendWithAuth(ctx, t, client, origin.URL, "Bearer "+sentinel)
	waited := time.Since(start)

	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d (%q), want 503", status, body)
	}
	if got := header.Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want the configured window, 1", got)
	}
	if got := header.Get("X-Should-Retry"); got != "true" {
		t.Fatalf("X-Should-Retry = %q, want true", got)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("upstream received %d requests, want none: the sentinel must not leave the proxy", n)
	}
	if waited < time.Second {
		t.Fatalf("answered after %v, want the retry window (1s) waited out first", waited)
	}
	if calls := resolver.resolveCount(); calls < 2 {
		t.Fatalf("resolver asked %d times, want it asked again inside the window", calls)
	}

	rows := waitForBlockedRows(t, dsn, 1)
	if len(rows) != 1 {
		t.Fatalf("blocked rows = %d, want one", len(rows))
	}
	row := rows[0]
	if row.Status != http.StatusServiceUnavailable {
		t.Fatalf("row status = %d, want 503", row.Status)
	}
	if !strings.HasPrefix(row.BlockedReason, "credential unavailable: ") || !strings.Contains(row.BlockedReason, "control plane unreachable") {
		t.Fatalf("row reason = %q, want it to say the credential could not be resolved, and why", row.BlockedReason)
	}
}

// A sentinel taken out of a sandbox's set stops resolving at the publish that
// removes it, however long its cached value had left.
func TestHTTPProxyStopsResolvingASentinelAtThePublishThatRemovesIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-ant-oat01-REMOVEDSENTINEL00000000000000000"
	const swapped = "sk-ant-oat01-REMOVEDREALVALUE0000000000000000"
	var mu sync.Mutex
	var seen []string
	origin := newOrigin(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	})
	defer origin.Close()

	resolver := &outageResolver{value: swapped}
	var cfg *Config
	server, client := startSecretProxy(ctx, t, sentinel, resolver, func(c *Config) { cfg = c })
	published := *cfg

	sendWithAuth(ctx, t, client, origin.URL, "Bearer "+sentinel)
	// The sandbox keeps a sentinel, so it is still swapped for: only this one
	// left its set.
	published.Secrets.Clients = []SecretClient{{ClientID: "sandbox-1", Sentinels: []string{"sk-ant-oat01-REPLACEMENT0000000000000000000"}}}
	if err := server.ApplyConfig(published); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	sendWithAuth(ctx, t, client, origin.URL, "Bearer "+sentinel)

	mu.Lock()
	defer mu.Unlock()
	want := []string{"Bearer " + swapped, "Bearer " + sentinel}
	if len(seen) != 2 || seen[0] != want[0] || seen[1] != want[1] {
		t.Fatalf("upstream saw %q, want %q: swapped before the publish, not after", seen, want)
	}
}

// rotationResolver hands sentinel A a short-lived value and then a second one,
// so the first is kept as A's displaced value, and B a long-lived one; until it
// is taken down, after which nothing resolves.
type rotationResolver struct {
	noGatewayHeaders
	outageResolver
	a, b string
	n    int
}

func (r *rotationResolver) Resolve(_ context.Context, req secrets.ResolveRequest) (secrets.ResolveResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.down {
		return secrets.ResolveResult{}, errors.New("control plane unreachable")
	}
	if req.Sentinel == r.b {
		return secrets.ResolveResult{Value: "B-VALUE", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	r.n++
	if r.n == 1 {
		return secrets.ResolveResult{Value: "A-FIRST", ExpiresAt: time.Now().Add(50 * time.Millisecond)}, nil
	}
	return secrets.ResolveResult{Value: "A-SECOND", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// The 401 retry's displaced-value candidate covers only the sentinels that
// rotated lately. With the control plane gone after the rejection, sending it
// would put every other sentinel on the wire as itself, so the upstream's 401 is
// passed through instead (ADR 26-10-14-482 §2).
func TestHTTPProxyRetrySendsNoSentinelThePreviousValueCannotCover(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const a = "sk-ant-oat01-ROTATEDSENTINELA00000000000000000"
	const b = "sk-ant-oat01-STEADYSENTINELB0000000000000000000"
	resolver := &rotationResolver{a: a, b: b}
	var attempts atomic.Int32
	var mu sync.Mutex
	var sawB []string
	origin := newOrigin(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		mu.Lock()
		sawB = append(sawB, r.Header.Get("X-Other"))
		mu.Unlock()
		if n == 1 {
			_, _ = io.WriteString(w, "ok")
			return
		}
		// The control plane goes away between the rejection and the retry.
		resolver.takeDown()
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer origin.Close()

	_, client := startSecretProxy(ctx, t, a, resolver, func(cfg *Config) {
		cfg.Secrets.Clients = []SecretClient{{ClientID: "sandbox-1", Sentinels: []string{a, b}}}
		cfg.Secrets.ResolveRetrySeconds = 1
	})

	send := func() int {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+a)
		req.Header.Set("X-Other", "Bearer "+b)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("client.Do() error = %v", err)
		}
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		return resp.StatusCode
	}
	if status := send(); status != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", status)
	}
	time.Sleep(100 * time.Millisecond) // A's first value expires
	if status := send(); status != http.StatusUnauthorized {
		t.Fatalf("second request status = %d, want the upstream's own 401", status)
	}

	mu.Lock()
	defer mu.Unlock()
	for i, got := range sawB {
		if got != "Bearer B-VALUE" {
			t.Fatalf("upstream request %d carried X-Other %q, want B's value and never its sentinel", i+1, got)
		}
	}
	if len(sawB) != 2 {
		t.Fatalf("upstream saw %d requests, want 2: the retry must not be sent", len(sawB))
	}
}

// ResolveRaw: outageResolver holds no raw secrets.
func (r *outageResolver) ResolveRaw(context.Context, secrets.RawRequest) (secrets.RawResult, error) {
	return secrets.RawResult{}, secrets.ErrDenied
}
