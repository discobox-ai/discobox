package secrets

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type fakeResolver struct {
	calls   atomic.Int64
	fn      func(ResolveRequest) (ResolveResult, error)
	lastReq ResolveRequest
}

func (f *fakeResolver) Resolve(_ context.Context, req ResolveRequest) (ResolveResult, error) {
	f.calls.Add(1)
	f.lastReq = req
	return f.fn(req)
}

func newRequest(t *testing.T, method, url string) *http.Request {
	t.Helper()
	return httptest.NewRequestWithContext(context.Background(), method, url, nil)
}

func TestSwapHeaderValue(t *testing.T) {
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: "sk-real-secret", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	sw := New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {"sk-ant-oat01-SENTINEL"}}})

	req := newRequest(t, http.MethodGet, "https://api.anthropic.com/v1/messages")
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-SENTINEL")

	res := sw.Apply(context.Background(), req, "sandbox-1")
	if !res.Swapped() {
		t.Fatal("expected swap")
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-real-secret" {
		t.Fatalf("Authorization = %q, want swapped value", got)
	}
	if len(res.Headers) != 1 || res.Headers[0] != "Authorization" {
		t.Fatalf("Headers = %v, want [Authorization]", res.Headers)
	}
	if resolver.lastReq.Host != "api.anthropic.com" {
		t.Fatalf("resolve host = %q, want api.anthropic.com", resolver.lastReq.Host)
	}
	if resolver.lastReq.ClientID != "sandbox-1" {
		t.Fatalf("resolve clientID = %q", resolver.lastReq.ClientID)
	}
}

func TestSwapQueryParam(t *testing.T) {
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: "REALKEY", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	sw := New(resolver, Config{
		Sentinels: map[string][]string{"sandbox-1": {"SENTINELKEY"}},
		ScanQuery: true,
	})

	req := newRequest(t, http.MethodGet, "https://example.com/api?key=SENTINELKEY&foo=bar")
	res := sw.Apply(context.Background(), req, "sandbox-1")
	if !res.Swapped() {
		t.Fatal("expected swap")
	}
	if got := req.URL.Query().Get("key"); got != "REALKEY" {
		t.Fatalf("key = %q, want REALKEY", got)
	}
	if got := req.URL.Query().Get("foo"); got != "bar" {
		t.Fatalf("foo = %q, want bar", got)
	}
	if len(res.QueryParams) != 1 || res.QueryParams[0] != "key" {
		t.Fatalf("QueryParams = %v, want [key]", res.QueryParams)
	}
}

func TestQueryNotScannedWhenDisabled(t *testing.T) {
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: "REALKEY", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	sw := New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {"SENTINELKEY"}}})

	req := newRequest(t, http.MethodGet, "https://example.com/api?key=SENTINELKEY")
	res := sw.Apply(context.Background(), req, "sandbox-1")
	if res.Swapped() {
		t.Fatal("query should not be scanned when ScanQuery is false")
	}
	if got := req.URL.Query().Get("key"); got != "SENTINELKEY" {
		t.Fatalf("key = %q, want unchanged sentinel", got)
	}
}

func TestDeniedLeavesSentinelAndCaches(t *testing.T) {
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{}, ErrDenied
	}}
	sw := New(resolver, Config{
		Sentinels:   map[string][]string{"sandbox-1": {"SENTINEL"}},
		NegativeTTL: time.Minute,
	})

	for i := 0; i < 3; i++ {
		req := newRequest(t, http.MethodGet, "https://evil.com/")
		req.Header.Set("Authorization", "Bearer SENTINEL")
		res := sw.Apply(context.Background(), req, "sandbox-1")
		if res.Swapped() {
			t.Fatal("denied sentinel must not be swapped")
		}
		if got := req.Header.Get("Authorization"); got != "Bearer SENTINEL" {
			t.Fatalf("Authorization = %q, want sentinel left in place", got)
		}
	}
	if calls := resolver.calls.Load(); calls != 1 {
		t.Fatalf("resolver called %d times, want 1 (denial cached)", calls)
	}
}

func TestPositiveResultCached(t *testing.T) {
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: "REAL", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	sw := New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {"SENTINEL"}}})

	for i := 0; i < 5; i++ {
		req := newRequest(t, http.MethodGet, "https://api.example.com/")
		req.Header.Set("Authorization", "Bearer SENTINEL")
		if res := sw.Apply(context.Background(), req, "sandbox-1"); !res.Swapped() {
			t.Fatal("expected swap")
		}
	}
	if calls := resolver.calls.Load(); calls != 1 {
		t.Fatalf("resolver called %d times, want 1 (value cached)", calls)
	}
}

func TestCacheKeyedByHost(t *testing.T) {
	resolver := &fakeResolver{fn: func(req ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: "real-" + req.Host, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	sw := New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {"SENTINEL"}}})

	for _, host := range []string{"a.example.com", "b.example.com"} {
		req := newRequest(t, http.MethodGet, "https://"+host+"/")
		req.Header.Set("Authorization", "Bearer SENTINEL")
		sw.Apply(context.Background(), req, "sandbox-1")
	}
	if calls := resolver.calls.Load(); calls != 2 {
		t.Fatalf("resolver called %d times, want 2 (distinct hosts)", calls)
	}
}

func TestTransientErrorNotCached(t *testing.T) {
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{}, context.DeadlineExceeded
	}}
	sw := New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {"SENTINEL"}}})

	for i := 0; i < 2; i++ {
		req := newRequest(t, http.MethodGet, "https://api.example.com/")
		req.Header.Set("Authorization", "Bearer SENTINEL")
		res := sw.Apply(context.Background(), req, "sandbox-1")
		if res.Swapped() {
			t.Fatal("transient error must not swap")
		}
		if len(res.Errors) != 1 {
			t.Fatalf("Errors = %v, want one transient error", res.Errors)
		}
	}
	if calls := resolver.calls.Load(); calls != 2 {
		t.Fatalf("resolver called %d times, want 2 (transient not cached)", calls)
	}
}

func TestInactiveClientSkipped(t *testing.T) {
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: "REAL", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	sw := New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {"SENTINEL"}}})

	if sw.Active("other") {
		t.Fatal("unknown client must be inactive")
	}
	req := newRequest(t, http.MethodGet, "https://api.example.com/")
	req.Header.Set("Authorization", "Bearer SENTINEL")
	if res := sw.Apply(context.Background(), req, "other"); res.Swapped() {
		t.Fatal("must not swap for unknown client")
	}
	if resolver.calls.Load() != 0 {
		t.Fatal("resolver must not be called for unknown client")
	}
}

func TestNilResolverNeverSwaps(t *testing.T) {
	sw := New(nil, Config{Sentinels: map[string][]string{"sandbox-1": {"SENTINEL"}}})
	if sw.Active("sandbox-1") {
		t.Fatal("nil resolver must be inactive")
	}
	req := newRequest(t, http.MethodGet, "https://api.example.com/")
	req.Header.Set("Authorization", "Bearer SENTINEL")
	if res := sw.Apply(context.Background(), req, "sandbox-1"); res.Swapped() {
		t.Fatal("nil resolver must not swap")
	}
}

func TestExpiredGrantRefetched(t *testing.T) {
	now := time.Unix(1000, 0)
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: "REAL", ExpiresAt: now.Add(30 * time.Second)}, nil
	}}
	sw := New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {"SENTINEL"}}})
	sw.now = func() time.Time { return now }

	apply := func() {
		req := newRequest(t, http.MethodGet, "https://api.example.com/")
		req.Header.Set("Authorization", "Bearer SENTINEL")
		sw.Apply(context.Background(), req, "sandbox-1")
	}
	apply()
	apply()
	if calls := resolver.calls.Load(); calls != 1 {
		t.Fatalf("resolver called %d times before expiry, want 1", calls)
	}
	now = now.Add(time.Minute) // past the grant expiry
	apply()
	if calls := resolver.calls.Load(); calls != 2 {
		t.Fatalf("resolver called %d times after expiry, want 2", calls)
	}
}
