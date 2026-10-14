package secrets

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

type fakeResolver struct {
	calls   atomic.Int64
	fn      func(ResolveRequest) (ResolveResult, error)
	lastReq ResolveRequest
	reports []ReportRequest
}

func (f *fakeResolver) Report(_ context.Context, req ReportRequest) error {
	f.reports = append(f.reports, req)
	return nil
}

func (f *fakeResolver) Gate(context.Context, GateRequest) (GateAdmission, error) {
	return GateAdmission{}, &GateRefusal{Reason: "no gate here"}
}

func (f *fakeResolver) Authorize(context.Context, AuthorizeRequest) (Verdict, error) {
	return Verdict{Allow: true}, nil
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
	sw := New(resolver, Config{
		Sentinels:    map[string][]string{"sandbox-1": {"SENTINEL"}},
		ResolveRetry: time.Nanosecond,
	})

	var asked int64
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
		calls := resolver.calls.Load()
		if calls <= asked {
			t.Fatalf("Apply %d did not ask the resolver: a transient failure was cached", i+1)
		}
		asked = calls
	}
}

// A control plane that comes back inside the retry window is waited for: the
// request swaps and reports no error (ADR 26-10-14-482 §2).
func TestTransientErrorRetriedWithinTheWindow(t *testing.T) {
	var failures atomic.Int64
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		if failures.Add(1) <= 2 {
			return ResolveResult{}, errors.New("connection refused")
		}
		return ResolveResult{Value: "REAL", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	sw := New(resolver, Config{
		Sentinels:    map[string][]string{"sandbox-1": {"SENTINEL"}},
		ResolveRetry: 5 * time.Second,
	})

	got, ok := swapAuth(t, sw, "sandbox-1", "SENTINEL")
	if !ok || got != "Bearer REAL" {
		t.Fatalf("Authorization = %q (swapped %v), want the value resolved on the third ask", got, ok)
	}
	if calls := resolver.calls.Load(); calls != 3 {
		t.Fatalf("resolver asked %d times, want 3", calls)
	}
}

// A failure that outlasts the window is reported in Errors, once the window
// has passed and not before.
func TestTransientErrorOutlastingTheWindowIsReported(t *testing.T) {
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{}, errors.New("connection refused")
	}}
	const window = 300 * time.Millisecond
	sw := New(resolver, Config{
		Sentinels:    map[string][]string{"sandbox-1": {"SENTINEL"}},
		ResolveRetry: window,
	})

	req := newRequest(t, http.MethodGet, "https://api.example.com/")
	req.Header.Set("Authorization", "Bearer SENTINEL")
	start := time.Now()
	res := sw.Apply(context.Background(), req, "sandbox-1")
	waited := time.Since(start)

	if res.Swapped() || len(res.Errors) != 1 {
		t.Fatalf("Result = %+v, want no swap and one error", res)
	}
	if waited < window {
		t.Fatalf("Apply returned after %v, before the %v window passed", waited, window)
	}
	if calls := resolver.calls.Load(); calls < 2 {
		t.Fatalf("resolver asked %d times, want it asked again inside the window", calls)
	}
}

// A denial is the control plane's answer, so it is neither retried nor an error:
// the sentinel stays in place and the request goes (ADR 26-10-14-482 §3).
func TestDenialIsNotRetried(t *testing.T) {
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{}, ErrDenied
	}}
	sw := New(resolver, Config{
		Sentinels:    map[string][]string{"sandbox-1": {"SENTINEL"}},
		ResolveRetry: 5 * time.Second,
	})

	req := newRequest(t, http.MethodGet, "https://api.example.com/")
	req.Header.Set("Authorization", "Bearer SENTINEL")
	res := sw.Apply(context.Background(), req, "sandbox-1")
	if res.Swapped() || len(res.Errors) != 0 {
		t.Fatalf("Result = %+v, want no swap and no error", res)
	}
	if calls := resolver.calls.Load(); calls != 1 {
		t.Fatalf("resolver asked %d times, want once", calls)
	}
}

// Reconfigure keeps what was resolved for every sentinel still in its client's
// set: a value, a denial, and the value a rotation displaced (ADR 26-10-14-482
// §1).
func TestReconfigureKeepsWhatAnUnchangedSentinelResolved(t *testing.T) {
	now := time.Unix(1000, 0)
	value := "OLD"
	down := false
	resolver := &fakeResolver{fn: func(req ResolveRequest) (ResolveResult, error) {
		if down {
			return ResolveResult{}, errors.New("connection refused")
		}
		if req.Sentinel == "DENIED" {
			return ResolveResult{}, ErrDenied
		}
		return ResolveResult{Value: value, ExpiresAt: now.Add(30 * time.Second)}, nil
	}}
	cfg := Config{
		Sentinels:    map[string][]string{"sandbox-1": {"SENTINEL", "DENIED"}},
		NegativeTTL:  time.Hour,
		ResolveRetry: time.Nanosecond,
	}
	sw := New(resolver, cfg)
	sw.now = func() time.Time { return now }

	swapAuth(t, sw, "sandbox-1", "SENTINEL")
	// Rotation: past the hard expiry, NEW is resolved and displaces OLD.
	now = now.Add(time.Minute)
	value = "NEW"
	swapAuth(t, sw, "sandbox-1", "SENTINEL")
	swapAuth(t, sw, "sandbox-1", "DENIED")
	asked := resolver.calls.Load()

	down = true
	cfg.Sentinels = map[string][]string{
		"sandbox-1": {"SENTINEL", "DENIED"},
		"sandbox-2": {"ELSEWHERE"},
	}
	sw.Reconfigure(cfg)

	if got, ok := swapAuth(t, sw, "sandbox-1", "SENTINEL"); !ok || got != "Bearer NEW" {
		t.Fatalf("Authorization = %q (swapped %v), want the cached value", got, ok)
	}
	if got, ok := applyPrevious(t, sw, "sandbox-1", "SENTINEL"); !ok || got != "Bearer OLD" {
		t.Fatalf("previous = %q (swapped %v), want the displaced value", got, ok)
	}
	if _, ok := swapAuth(t, sw, "sandbox-1", "DENIED"); ok {
		t.Fatal("a cached denial swapped")
	}
	if calls := resolver.calls.Load(); calls != asked {
		t.Fatalf("resolver asked %d more times after the reconfigure, want none", calls-asked)
	}
}

// A sentinel that leaves its client's set is forgotten for that client: put
// back, it resolves again, and its displaced value is gone.
func TestReconfigureForgetsARemovedSentinel(t *testing.T) {
	now := time.Unix(1000, 0)
	value := "OLD"
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: value, ExpiresAt: now.Add(30 * time.Second)}, nil
	}}
	with := Config{Sentinels: map[string][]string{"sandbox-1": {"SENTINEL", "OTHER"}}}
	sw := New(resolver, with)
	sw.now = func() time.Time { return now }
	swapAuth(t, sw, "sandbox-1", "SENTINEL")
	now = now.Add(time.Minute)
	value = "NEW"
	swapAuth(t, sw, "sandbox-1", "SENTINEL")
	if _, ok := applyPrevious(t, sw, "sandbox-1", "SENTINEL"); !ok {
		t.Fatal("the rotation left no displaced value to forget")
	}

	sw.Reconfigure(Config{Sentinels: map[string][]string{"sandbox-1": {"OTHER"}}})
	if _, ok := swapAuth(t, sw, "sandbox-1", "SENTINEL"); ok {
		t.Fatal("a removed sentinel still swapped")
	}

	sw.Reconfigure(with)
	asked := resolver.calls.Load()
	if _, ok := applyPrevious(t, sw, "sandbox-1", "SENTINEL"); ok {
		t.Fatal("the displaced value outlived the sentinel's removal")
	}
	swapAuth(t, sw, "sandbox-1", "SENTINEL")
	if calls := resolver.calls.Load(); calls != asked+1 {
		t.Fatalf("resolver asked %d times after the sentinel came back, want once", calls-asked)
	}
}

// A resolve still in flight when its sentinel is removed does not store its
// answer, so the sentinel cannot come back with it.
func TestReconfigureDropsALateAnswerForARemovedSentinel(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		select {
		case entered <- struct{}{}:
			<-release
		default:
		}
		return ResolveResult{Value: "REAL", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	with := Config{Sentinels: map[string][]string{"sandbox-1": {"SENTINEL"}}}
	sw := New(resolver, with)

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := newRequest(t, http.MethodGet, "https://api.example.com/")
		req.Header.Set("Authorization", "Bearer SENTINEL")
		sw.Apply(context.Background(), req, "sandbox-1")
	}()
	<-entered
	sw.Reconfigure(Config{Sentinels: map[string][]string{"sandbox-1": {"OTHER"}}})
	close(release)
	<-done

	sw.Reconfigure(with)
	swapAuth(t, sw, "sandbox-1", "SENTINEL")
	if calls := resolver.calls.Load(); calls != 2 {
		t.Fatalf("resolver asked %d times, want the late answer discarded and the sentinel resolved again", calls)
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

// swapAuth applies the swapper to a request bearing sentinel and returns the
// resulting Authorization header and whether a swap occurred.
func swapAuth(t *testing.T, sw *Swapper, clientID, sentinel string) (string, bool) {
	t.Helper()
	req := newRequest(t, http.MethodGet, "https://api.example.com/")
	req.Header.Set("Authorization", "Bearer "+sentinel)
	res := sw.Apply(context.Background(), req, clientID)
	return req.Header.Get("Authorization"), res.Swapped()
}

// waitFor polls cond until it holds or the deadline passes, for observing a
// background refresh that completes asynchronously.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

// Past the soft refresh interval but within the hard grant expiry, the cached
// value is served immediately and a background refresh updates it.
func TestSoftRefreshUpdatesValueInBackground(t *testing.T) {
	base := time.Unix(1000, 0)
	now := base
	var calls atomic.Int64
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		// First value differs from later refreshes so the update is observable.
		v := "v2"
		if calls.Add(1) == 1 {
			v = "v1"
		}
		return ResolveResult{Value: v, ExpiresAt: base.Add(time.Hour)}, nil
	}}
	sw := New(resolver, Config{
		Sentinels:       map[string][]string{"sandbox-1": {"SENTINEL"}},
		RefreshInterval: 30 * time.Second,
	})
	sw.now = func() time.Time { return now }

	if v, _ := swapAuth(t, sw, "sandbox-1", "SENTINEL"); v != "Bearer v1" {
		t.Fatalf("primed swap = %q, want Bearer v1", v)
	}
	// Enter the soft window; the cached value is still served synchronously.
	now = base.Add(31 * time.Second)
	if v, _ := swapAuth(t, sw, "sandbox-1", "SENTINEL"); v != "Bearer v1" {
		t.Fatalf("soft-window swap = %q, want cached Bearer v1", v)
	}
	// The background refresh eventually replaces the value.
	waitFor(t, func() bool {
		v, _ := swapAuth(t, sw, "sandbox-1", "SENTINEL")
		return v == "Bearer v2"
	})
	if calls := resolver.calls.Load(); calls != 2 {
		t.Fatalf("resolver called %d times, want 2 (prime + one refresh)", calls)
	}
}

// A transient background-refresh failure keeps serving the cached value until
// its hard expiry, preserving availability when the control plane is down.
func TestSoftRefreshTransientFailureKeepsValue(t *testing.T) {
	base := time.Unix(1000, 0)
	now := base
	var calls atomic.Int64
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		if calls.Add(1) == 1 {
			return ResolveResult{Value: "v1", ExpiresAt: base.Add(time.Hour)}, nil
		}
		return ResolveResult{}, context.DeadlineExceeded // transient
	}}
	sw := New(resolver, Config{
		Sentinels:       map[string][]string{"sandbox-1": {"SENTINEL"}},
		RefreshInterval: 30 * time.Second,
	})
	sw.now = func() time.Time { return now }

	swapAuth(t, sw, "sandbox-1", "SENTINEL")
	now = base.Add(31 * time.Second)
	// Triggers a background refresh that fails transiently.
	swapAuth(t, sw, "sandbox-1", "SENTINEL")
	waitFor(t, func() bool { return calls.Load() >= 2 })

	// The cached value is retained despite the failed refresh.
	if v, swapped := swapAuth(t, sw, "sandbox-1", "SENTINEL"); !swapped || v != "Bearer v1" {
		t.Fatalf("post-failure swap = %q swapped=%v, want Bearer v1 true", v, swapped)
	}
}

// An authoritative denial during a background refresh invalidates the cached
// value promptly rather than serving it until hard expiry.
func TestSoftRefreshDeniedInvalidates(t *testing.T) {
	base := time.Unix(1000, 0)
	now := base
	var calls atomic.Int64
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		if calls.Add(1) == 1 {
			return ResolveResult{Value: "v1", ExpiresAt: base.Add(time.Hour)}, nil
		}
		return ResolveResult{}, ErrDenied
	}}
	sw := New(resolver, Config{
		Sentinels:       map[string][]string{"sandbox-1": {"SENTINEL"}},
		RefreshInterval: 30 * time.Second,
	})
	sw.now = func() time.Time { return now }

	swapAuth(t, sw, "sandbox-1", "SENTINEL")
	now = base.Add(31 * time.Second)
	swapAuth(t, sw, "sandbox-1", "SENTINEL") // triggers refresh that is denied

	// After the denial the sentinel is left in place.
	waitFor(t, func() bool {
		v, swapped := swapAuth(t, sw, "sandbox-1", "SENTINEL")
		return !swapped && v == "Bearer SENTINEL"
	})
}

// applyPrevious runs ApplyPrevious over a request bearing sentinel and reports
// the resulting Authorization header.
func applyPrevious(t *testing.T, sw *Swapper, clientID, sentinel string) (string, bool) {
	t.Helper()
	req := newRequest(t, http.MethodGet, "https://api.example.com/")
	req.Header.Set("Authorization", "Bearer "+sentinel)
	res := sw.ApplyPrevious(req, clientID)
	return req.Header.Get("Authorization"), res.Swapped()
}

// When a rotation replaces a value, the one it displaced stays available: the
// upstream may not have started honoring the new credential yet, and the
// displaced one is the only other thing to try.
func TestPreviousValueAvailableAfterRotation(t *testing.T) {
	now := time.Unix(1000, 0)
	value := "FIRST"
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: value, ExpiresAt: now.Add(30 * time.Second)}, nil
	}}
	sw := New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {"SENTINEL"}}})
	sw.now = func() time.Time { return now }

	if got, _ := swapAuth(t, sw, "sandbox-1", "SENTINEL"); got != "Bearer FIRST" {
		t.Fatalf("Authorization = %q, want the first value", got)
	}
	if _, ok := applyPrevious(t, sw, "sandbox-1", "SENTINEL"); ok {
		t.Fatal("ApplyPrevious swapped with nothing displaced yet")
	}

	// Rotation: past the hard expiry, the next use resolves and stores anew.
	now = now.Add(time.Minute)
	value = "SECOND"
	if got, _ := swapAuth(t, sw, "sandbox-1", "SENTINEL"); got != "Bearer SECOND" {
		t.Fatalf("Authorization = %q, want the rotated value", got)
	}
	if got, ok := applyPrevious(t, sw, "sandbox-1", "SENTINEL"); !ok || got != "Bearer FIRST" {
		t.Fatalf("ApplyPrevious = %q (swapped %v), want the displaced value", got, ok)
	}

	// Invalidating the rejected value must not take the fallback with it: the
	// retry path invalidates before it re-resolves.
	sw.Invalidate("sandbox-1", "api.example.com")
	if got, ok := applyPrevious(t, sw, "sandbox-1", "SENTINEL"); !ok || got != "Bearer FIRST" {
		t.Fatalf("ApplyPrevious after Invalidate = %q (swapped %v), want the displaced value", got, ok)
	}

	now = now.Add(previousValueGrace + time.Second)
	if _, ok := applyPrevious(t, sw, "sandbox-1", "SENTINEL"); ok {
		t.Fatal("ApplyPrevious swapped past the grace window")
	}
}

// Invalidate drops the cached value so the next use resolves again.
func TestInvalidateForcesReresolve(t *testing.T) {
	now := time.Unix(1000, 0)
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: "REAL", ExpiresAt: now.Add(time.Hour)}, nil
	}}
	sw := New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {"SENTINEL"}}})
	sw.now = func() time.Time { return now }

	swapAuth(t, sw, "sandbox-1", "SENTINEL")
	swapAuth(t, sw, "sandbox-1", "SENTINEL")
	if calls := resolver.calls.Load(); calls != 1 {
		t.Fatalf("resolver called %d times, want the second use served from cache", calls)
	}
	sw.Invalidate("sandbox-1", "api.example.com:443")
	swapAuth(t, sw, "sandbox-1", "SENTINEL")
	if calls := resolver.calls.Load(); calls != 2 {
		t.Fatalf("resolver called %d times after Invalidate, want a fresh resolve", calls)
	}
}

// The use ID is the join to the control plane's verdict trail (ADR 0130 §3), so
// it has to survive the swap that spends it.
func TestSwapReportsUseID(t *testing.T) {
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: "REALKEY", UseID: "use_abc", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	sw := New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {"SENTINELKEY"}}})

	req := newRequest(t, http.MethodGet, "https://example.com/api")
	req.Header.Set("Authorization", "Bearer SENTINELKEY")

	res := sw.Apply(context.Background(), req, "sandbox-1")
	if !res.Swapped() {
		t.Fatal("expected swap")
	}
	if !slices.Equal(res.UseIDs, []string{"use_abc"}) {
		t.Fatalf("UseIDs = %v, want [use_abc]", res.UseIDs)
	}
}

// A cached value must still name its use: the audit row is written per request,
// not per resolve, and only the first request of a grant resolves at all.
func TestSwapReportsUseIDFromCache(t *testing.T) {
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: "REALKEY", UseID: "use_abc", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	sw := New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {"SENTINELKEY"}}})

	for i := range 2 {
		req := newRequest(t, http.MethodGet, "https://example.com/api")
		req.Header.Set("Authorization", "Bearer SENTINELKEY")
		res := sw.Apply(context.Background(), req, "sandbox-1")
		if !slices.Equal(res.UseIDs, []string{"use_abc"}) {
			t.Fatalf("request %d: UseIDs = %v, want [use_abc]", i, res.UseIDs)
		}
	}
	if calls := resolver.calls.Load(); calls != 1 {
		t.Fatalf("resolver calls = %d, want 1 (second request should hit the cache)", calls)
	}
}

// A sentinel outside the agent credentials protocol has no approved use, and
// the empty ID must not reach the result as an element.
func TestSwapWithoutUseIDReportsNone(t *testing.T) {
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: "REALKEY", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	sw := New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {"SENTINELKEY"}}})

	req := newRequest(t, http.MethodGet, "https://example.com/api")
	req.Header.Set("Authorization", "Bearer SENTINELKEY")

	res := sw.Apply(context.Background(), req, "sandbox-1")
	if !res.Swapped() {
		t.Fatal("expected swap")
	}
	if len(res.UseIDs) != 0 {
		t.Fatalf("UseIDs = %v, want none", res.UseIDs)
	}
}

// Git's basic auth puts two sentinels in one header, so one request can spend
// two approved uses.
func TestSwapReportsEveryUseID(t *testing.T) {
	resolver := &fakeResolver{fn: func(req ResolveRequest) (ResolveResult, error) {
		switch req.Sentinel {
		case "SENTINELUSER":
			return ResolveResult{Value: "real-user", UseID: "use_user", ExpiresAt: time.Now().Add(time.Hour)}, nil
		case "SENTINELPASS":
			return ResolveResult{Value: "real-pass", UseID: "use_pass", ExpiresAt: time.Now().Add(time.Hour)}, nil
		}
		return ResolveResult{}, ErrDenied
	}}
	sw := New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {"SENTINELUSER", "SENTINELPASS"}}})

	req := newRequest(t, http.MethodGet, "https://github.com/org/repo.git/info/refs")
	req.Header.Set("Authorization", basicAuth("SENTINELUSER:SENTINELPASS"))

	res := sw.Apply(context.Background(), req, "sandbox-1")
	if !res.Swapped() {
		t.Fatal("expected swap")
	}
	if !slices.Equal(res.UseIDs, []string{"use_pass", "use_user"}) {
		t.Fatalf("UseIDs = %v, want [use_pass use_user]", res.UseIDs)
	}
}

// Every swapped value names its secret, an ordinary sentinel's as much as one
// taken under an approved use — which has no use to name — and a cached value
// as much as one just resolved (ADR 26-10-01-240).
func TestSwapReportsSecretIDForEverySentinel(t *testing.T) {
	resolver := &fakeResolver{fn: func(req ResolveRequest) (ResolveResult, error) {
		switch req.Sentinel {
		case "SENTINELUSER":
			return ResolveResult{Value: "real-user", SecretID: "sec_user", ExpiresAt: time.Now().Add(time.Hour)}, nil
		case "SENTINELPASS":
			return ResolveResult{Value: "real-pass", UseID: "use_pass", SecretID: "sec_pass", ExpiresAt: time.Now().Add(time.Hour)}, nil
		}
		return ResolveResult{}, ErrDenied
	}}
	sw := New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {"SENTINELUSER", "SENTINELPASS"}}})

	for i := range 2 {
		req := newRequest(t, http.MethodGet, "https://github.com/org/repo.git/info/refs")
		req.Header.Set("Authorization", basicAuth("SENTINELUSER:SENTINELPASS"))
		res := sw.Apply(context.Background(), req, "sandbox-1")
		if !slices.Equal(res.SecretIDs, []string{"sec_pass", "sec_user"}) {
			t.Fatalf("request %d: SecretIDs = %v, want both secrets", i, res.SecretIDs)
		}
		if !slices.Equal(res.UseIDs, []string{"use_pass"}) {
			t.Fatalf("request %d: UseIDs = %v, want only the approved use", i, res.UseIDs)
		}
	}
	if calls := resolver.calls.Load(); calls != 2 {
		t.Fatalf("resolver calls = %d, want 2 (the second request should hit the cache)", calls)
	}
}

// The retry with the value a rotation displaced spends the same secret, and
// its row names it.
func TestApplyPreviousReportsTheSecret(t *testing.T) {
	now := time.Unix(1000, 0)
	value := "FIRST"
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: value, SecretID: "sec_key", ExpiresAt: now.Add(30 * time.Second)}, nil
	}}
	sw := New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {"SENTINEL"}}})
	sw.now = func() time.Time { return now }
	swapAuth(t, sw, "sandbox-1", "SENTINEL")
	now = now.Add(time.Minute)
	value = "SECOND"
	swapAuth(t, sw, "sandbox-1", "SENTINEL")

	req := newRequest(t, http.MethodGet, "https://api.example.com/")
	req.Header.Set("Authorization", "Bearer SENTINEL")
	res := sw.ApplyPrevious(req, "sandbox-1")
	if !res.Swapped() || !slices.Equal(res.SecretIDs, []string{"sec_key"}) {
		t.Fatalf("ApplyPrevious swapped %v with SecretIDs %v, want the displaced value's secret", res.Swapped(), res.SecretIDs)
	}
}

// What Match reports is what Apply would swap. The two walk the same surface
// through the same scan, and a sentinel Apply substitutes without Match having
// named it would be a credential sent on a request nothing authorized
// (ADR 26-09-22-838 §4).
func TestMatchReportsEverythingApplyWouldSwap(t *testing.T) {
	const (
		header = "sk-ant-oat01-HEADER"
		query  = "sk-ant-oat01-QUERY"
		basic  = "sk-ant-oat01-BASIC"
		unused = "sk-ant-oat01-UNUSED"
	)
	sentinels := []string{header, query, basic, unused}
	build := func(t *testing.T) *http.Request {
		t.Helper()
		req := newRequest(t, http.MethodPost, "https://api.github.com/repos?token="+query)
		req.Header.Set("Authorization", "Bearer "+header)
		req.Header.Set("X-Other", "Basic "+base64.StdEncoding.EncodeToString([]byte("git:"+basic)))
		return req
	}

	resolver := &fakeResolver{fn: func(req ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: "real-" + req.Sentinel, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	sw := New(resolver, Config{ScanQuery: true, Sentinels: map[string][]string{"sandbox-1": sentinels}})

	matched := sw.Match(build(t), "sandbox-1")
	slices.Sort(matched)
	want := []string{basic, header, query}
	slices.Sort(want)
	if !slices.Equal(matched, want) {
		t.Fatalf("Match() = %v, want the three sentinels the request carries", matched)
	}
	if resolver.calls.Load() != 0 {
		t.Fatalf("Match() resolved %d sentinels, want a read that resolves nothing", resolver.calls.Load())
	}

	swapped := build(t)
	res := sw.Apply(context.Background(), swapped, "sandbox-1")
	slices.Sort(res.Sentinels)
	if !slices.Equal(res.Sentinels, want) {
		t.Fatalf("Apply() swapped %v, want the same set Match reported", res.Sentinels)
	}
}

// Match leaves the request exactly as it found it: it is asked before anything
// is authorized, on a request that may yet be refused.
func TestMatchDoesNotTouchTheRequest(t *testing.T) {
	const sentinel = "sk-ant-oat01-SENTINEL"
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: "sk-real-secret", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	sw := New(resolver, Config{ScanQuery: true, Sentinels: map[string][]string{"sandbox-1": {sentinel}}})

	req := newRequest(t, http.MethodGet, "https://api.github.com/user?token="+sentinel)
	req.Header.Set("Authorization", "Bearer "+sentinel)
	encoded := base64.StdEncoding.EncodeToString([]byte("git:" + sentinel))
	req.Header.Set("X-Other", "Basic "+encoded)

	if got := sw.Match(req, "sandbox-1"); len(got) != 1 || got[0] != sentinel {
		t.Fatalf("Match() = %v, want the one sentinel", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+sentinel {
		t.Fatalf("Authorization = %q, want it untouched", got)
	}
	if got := req.Header.Get("X-Other"); got != "Basic "+encoded {
		t.Fatalf("X-Other = %q, want the token byte-for-byte", got)
	}
	if got := req.URL.Query().Get("token"); got != sentinel {
		t.Fatalf("token = %q, want it untouched", got)
	}
}

// A destination with a port is bound to the same use as one without: the
// authorizer is told the host resolution will ask about.
func TestAuthorizeStatesTheHostResolutionWill(t *testing.T) {
	resolver := &hostRecordingResolver{}
	sw := New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {"sk-ant-oat01-SENTINEL"}}})
	if _, err := sw.Authorize(context.Background(), AuthorizeRequest{
		ClientID: "sandbox-1",
		Host:     "api.github.com:8443",
		URL:      "https://api.github.com:8443/user",
	}); err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if resolver.host != "api.github.com" {
		t.Fatalf("authorizer was told host %q, want it without the port", resolver.host)
	}
}

type hostRecordingResolver struct {
	fakeResolver
	host string
}

func (r *hostRecordingResolver) Authorize(_ context.Context, req AuthorizeRequest) (Verdict, error) {
	r.host = req.Host
	return Verdict{Allow: true}, nil
}

// An empty sentinel is not a sentinel. The resolving scan never substitutes
// for one because it never resolves; the redacting scan cannot decline, so it
// has to refuse the empty string itself or it would rewrite every value into
// markers.
func TestRedactIgnoresAnEmptySentinel(t *testing.T) {
	const value = "Bearer sk-ant-oat01-SENTINEL"
	if got := Redact(value, []string{""}, "<redacted>"); got != value {
		t.Fatalf("Redact() = %q, want the value untouched", got)
	}
	if got := Redact(value, []string{"", "sk-ant-oat01-SENTINEL"}, "<redacted>"); got != "Bearer <redacted>" {
		t.Fatalf("Redact() = %q, want the real sentinel still redacted", got)
	}
}

func TestReportScopeCovers(t *testing.T) {
	scope := ReportScope{
		{Host: "api.anthropic.com", Paths: []string{"/v1/messages", "/v1/complete/"}},
		{Host: "example.com"},
	}
	for _, tc := range []struct {
		host, path string
		want       bool
	}{
		{"api.anthropic.com", "/v1/messages", true},
		{"api.anthropic.com", "/v1/messages/count_tokens", true},
		{"api.anthropic.com", "/v1/complete", true},
		{"api.anthropic.com", "/v1/messagesX", false},
		{"api.anthropic.com", "/v1/code/runners/self-hosted/spawn-hints/poll", false},
		{"api.anthropic.com", "/api/event_logging/v2/batch", false},
		{"console.anthropic.com", "/v1/messages", false},
		{"example.com", "/anything", true},
		{"api.example.com", "", true},
	} {
		if got := scope.Covers(tc.host, tc.path); got != tc.want {
			t.Errorf("Covers(%q, %q) = %v, want %v", tc.host, tc.path, got, tc.want)
		}
	}
	if !(ReportScope(nil)).Covers("anywhere.test", "/any") {
		t.Error("an empty scope must cover every request")
	}
}

// Whether a request answers for a credential is asked of every request that
// carries it, so the scope has to come back from the cache as well as from
// the resolve.
func TestSwapNamesTheSentinelsARequestAnswersFor(t *testing.T) {
	resolver := &fakeResolver{fn: func(req ResolveRequest) (ResolveResult, error) {
		res := ResolveResult{Value: "real", ExpiresAt: time.Now().Add(time.Hour)}
		if req.Sentinel == "SENTINELSCOPED" {
			res.ReportScope = ReportScope{{Host: "api.example.com", Paths: []string{"/v1/messages"}}}
		}
		return res, nil
	}}
	sw := New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {"SENTINELSCOPED", "SENTINELPLAIN"}}})

	apply := func(url string) Result {
		req := newRequest(t, http.MethodPost, url)
		req.Header.Set("Authorization", "Bearer SENTINELSCOPED")
		req.Header.Set("X-Other", "SENTINELPLAIN")
		return sw.Apply(context.Background(), req, "sandbox-1")
	}
	for i := range 2 {
		res := apply("https://api.example.com/v1/other")
		if !slices.Equal(res.Sentinels, []string{"SENTINELPLAIN", "SENTINELSCOPED"}) || !slices.Equal(res.Reportable, []string{"SENTINELPLAIN"}) {
			t.Fatalf("request %d outside the scope: Sentinels = %v Reportable = %v", i, res.Sentinels, res.Reportable)
		}
		res = apply("https://api.example.com/v1/messages?beta=true")
		if !slices.Equal(res.Reportable, []string{"SENTINELPLAIN", "SENTINELSCOPED"}) {
			t.Fatalf("request %d inside the scope: Reportable = %v, want both", i, res.Reportable)
		}
	}
}

// A control plane that hangs instead of refusing is bounded by the window as
// well: each ask is given a deadline, never less than resolveAttemptFloor.
func TestAHungResolveIsBoundedByTheWindow(t *testing.T) {
	resolver := &ctxResolver{}
	sw := New(resolver, Config{
		Sentinels:    map[string][]string{"sandbox-1": {"SENTINEL"}},
		ResolveRetry: 100 * time.Millisecond,
	})

	req := newRequest(t, http.MethodGet, "https://api.example.com/")
	req.Header.Set("Authorization", "Bearer SENTINEL")
	start := time.Now()
	res := sw.Apply(context.Background(), req, "sandbox-1")
	waited := time.Since(start)

	if len(res.Errors) != 1 {
		t.Fatalf("Errors = %v, want the hung ask reported", res.Errors)
	}
	if waited > resolveAttemptFloor+time.Second {
		t.Fatalf("Apply held the request %v, want about the attempt floor (%v)", waited, resolveAttemptFloor)
	}
}

// ctxResolver resolves nothing: it waits for the ask's context to end.
type ctxResolver struct{ fakeResolver }

func (r *ctxResolver) Resolve(ctx context.Context, _ ResolveRequest) (ResolveResult, error) {
	<-ctx.Done()
	return ResolveResult{}, ctx.Err()
}
