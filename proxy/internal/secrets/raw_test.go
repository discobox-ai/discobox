package secrets

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const rawValue = "AKIAREALSECRETVALUE/with+chars="

// rawResolver answers ResolveRaw from a map it can change under the test.
type rawResolver struct {
	fakeResolver
	mu     sync.Mutex
	values map[string]RawResult
	err    error
	asked  int
}

func newRawResolver(values map[string]RawResult) *rawResolver {
	r := &rawResolver{values: values}
	r.raw = func(req RawRequest) (RawResult, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.asked++
		if r.err != nil {
			return RawResult{}, r.err
		}
		if v, ok := r.values[req.Sentinel]; ok {
			return v, nil
		}
		return RawResult{}, ErrDenied
	}
	return r
}

func (r *rawResolver) set(sentinel string, result RawResult, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ok {
		r.values[sentinel] = result
	} else {
		delete(r.values, sentinel)
	}
}

func (r *rawResolver) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *rawResolver) askedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.asked
}

// testClock is a clock the test moves while the watch's background resolves
// may read it.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func rawSwapper(t *testing.T, resolver *rawResolver, clock *testClock) *Swapper {
	t.Helper()
	sw := New(resolver, Config{})
	if clock != nil {
		sw.now = clock.Now
	}
	sw.Reconfigure(Config{Raw: map[string][]string{"sb-1": {"RAW-SENTINEL"}}})
	return sw
}

// The value is handed out by its binding's sentinel, and it is what every
// request of its client is watched for.
func TestRawValuesAreHandedOutAndWatchedFor(t *testing.T) {
	resolver := newRawResolver(map[string]RawResult{"RAW-SENTINEL": {Value: rawValue, SecretID: "sec_1", BindingID: "sbd_1"}})
	sw := rawSwapper(t, resolver, nil)
	values := sw.RawValues(context.Background(), "sb-1")
	if len(values) != 1 || values[0].Value != rawValue || values[0].BindingID != "sbd_1" || values[0].Sentinel != "RAW-SENTINEL" {
		t.Fatalf("values = %+v, want the binding's value", values)
	}
	if got := sw.RawValues(context.Background(), "sb-2"); len(got) != 0 {
		t.Fatalf("another client's values = %+v, want none", got)
	}
	set := sw.Raw("sb-1")
	if set == nil {
		t.Fatal("no raw set for the client holding a value")
	}
	if sw.Raw("sb-2") != nil {
		t.Fatal("a raw set for a client holding nothing")
	}
}

// Every spelling the scan knows finds the value: itself, base64 in both
// alphabets padded and not, percent-encoded, and inside a Basic token.
func TestRawSetFindsEveryForm(t *testing.T) {
	set := newRawSet([]rawWatched{{sentinel: "S", secretID: "sec_1", bindingID: "sbd_1", value: rawValue}})
	data := []byte(rawValue)
	cases := map[string]string{
		"literal":       "Bearer " + rawValue,
		"std":           base64.StdEncoding.EncodeToString(data),
		"raw std":       base64.RawStdEncoding.EncodeToString(data),
		"url":           base64.URLEncoding.EncodeToString(data),
		"raw url":       base64.RawURLEncoding.EncodeToString(data),
		"query escaped": "key=" + url.QueryEscape(rawValue),
		"path escaped":  "/v1/" + url.PathEscape(rawValue),
		"basic":         "Basic " + base64.StdEncoding.EncodeToString([]byte("user:"+rawValue)),
	}
	for name, text := range cases {
		hits := set.Find(text)
		if len(hits) != 1 || hits[0].BindingID != "sbd_1" {
			t.Errorf("%s: Find(%q) = %+v, want the binding", name, text, hits)
		}
		if redacted := set.Redact(text); strings.Contains(redacted, rawValue) || len(set.Find(redacted)) != 0 {
			t.Errorf("%s: Redact(%q) = %q, still carries the value", name, text, redacted)
		}
	}
	if hits := set.Find("Bearer something-else-entirely"); len(hits) != 0 {
		t.Fatalf("Find on unrelated text = %+v", hits)
	}
	if hits := set.FindBytes([]byte(`{"password":"` + rawValue + `"}`)); len(hits) != 1 {
		t.Fatalf("FindBytes on a body = %+v, want the binding", hits)
	}
}

// A value too short to tell from ordinary text is neither watched for nor
// redacted.
func TestShortRawValuesAreNotWatched(t *testing.T) {
	resolver := newRawResolver(map[string]RawResult{"RAW-SENTINEL": {Value: "short", SecretID: "sec_1"}})
	sw := rawSwapper(t, resolver, nil)
	if got := sw.RawValues(context.Background(), "sb-1"); len(got) != 1 {
		t.Fatalf("values = %+v, want the short value still handed out", got)
	}
	if set := sw.Raw("sb-1"); set != nil {
		t.Fatalf("raw set = %+v, want nothing watched", set)
	}
}

// A value is asked for again once its refresh is due; a rotated one is
// handed out new and the old one is still watched for, since a process may
// hold it.
func TestRawValueRefreshesAndKeepsWatchingTheOldOne(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	resolver := newRawResolver(map[string]RawResult{"RAW-SENTINEL": {Value: rawValue, SecretID: "sec_1"}})
	sw := rawSwapper(t, resolver, clock)
	sw.RawValues(context.Background(), "sb-1")
	asked := resolver.askedCount()
	sw.RawValues(context.Background(), "sb-1")
	if resolver.askedCount() != asked {
		t.Fatalf("asked again before the refresh was due")
	}
	rotated := "ROTATEDSECRETVALUE-0123456789"
	resolver.set("RAW-SENTINEL", RawResult{Value: rotated, SecretID: "sec_1"}, true)
	clock.Add(defaultRefreshInterval + time.Second)
	values := sw.RawValues(context.Background(), "sb-1")
	if len(values) != 1 || values[0].Value != rotated {
		t.Fatalf("values = %+v, want the rotated value", values)
	}
	set := sw.Raw("sb-1")
	if len(set.Find(rotated)) != 1 || len(set.Find(rawValue)) != 1 {
		t.Fatal("want both the rotated value and the one it replaced watched for")
	}
}

// A denial takes the value out of what is handed over, and the value is still
// watched for; an unreachable resolver serves what is held until it lapses.
func TestRawValueDeniedOrUnreachable(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	resolver := newRawResolver(map[string]RawResult{"RAW-SENTINEL": {Value: rawValue, SecretID: "sec_1", ExpiresAt: clock.Now().Add(time.Hour)}})
	sw := rawSwapper(t, resolver, clock)
	sw.RawValues(context.Background(), "sb-1")

	resolver.setErr(errors.New("control plane down"))
	clock.Add(defaultRefreshInterval + time.Second)
	if values := sw.RawValues(context.Background(), "sb-1"); len(values) != 1 {
		t.Fatalf("values while unreachable = %+v, want the one held", values)
	}
	clock.Add(2 * time.Hour)
	if values := sw.RawValues(context.Background(), "sb-1"); len(values) != 0 {
		t.Fatalf("values past their lapse = %+v, want none", values)
	}

	resolver.setErr(nil)
	resolver.set("RAW-SENTINEL", RawResult{}, false)
	if values := sw.RawValues(context.Background(), "sb-1"); len(values) != 0 {
		t.Fatalf("values once denied = %+v, want none", values)
	}
	if set := sw.Raw("sb-1"); len(set.Find(rawValue)) != 1 {
		t.Fatal("a denied value is no longer watched for")
	}
}

// A sentinel leaving the client's set retires its value, which stays watched
// for through its grace and is then forgotten.
func TestRawSentinelLeavingRetiresItsValue(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	resolver := newRawResolver(map[string]RawResult{"RAW-SENTINEL": {Value: rawValue, SecretID: "sec_1"}})
	sw := rawSwapper(t, resolver, clock)
	sw.RawValues(context.Background(), "sb-1")
	sw.Reconfigure(Config{})
	if values := sw.RawValues(context.Background(), "sb-1"); len(values) != 0 {
		t.Fatalf("values after the sentinel left = %+v", values)
	}
	if set := sw.Raw("sb-1"); len(set.Find(rawValue)) != 1 {
		t.Fatal("a retired value is no longer watched for")
	}
	clock.Add(rawRetiredGrace + time.Minute)
	sw.Reconfigure(Config{})
	if set := sw.Raw("sb-1"); set != nil {
		t.Fatalf("raw set past the grace = %+v, want nothing", set)
	}
}

// A request waits for the client's configured raw values: a denial is an
// answer, and is not asked about again within the negative TTL; an outage
// that outlasts the window is a failure the request is refused over.
func TestAwaitRawAnswersFromTheConfiguredSet(t *testing.T) {
	resolver := newRawResolver(map[string]RawResult{})
	sw := New(resolver, Config{ResolveRetry: 50 * time.Millisecond})
	sw.Reconfigure(Config{ResolveRetry: 50 * time.Millisecond, Raw: map[string][]string{"sb-1": {"RAW-SENTINEL"}}})
	if errs := sw.AwaitRaw(context.Background(), "sb-1"); len(errs) != 0 {
		t.Fatalf("denied: errs = %v, want none", errs)
	}
	asked := resolver.askedCount()
	sw.AwaitRaw(context.Background(), "sb-1")
	if resolver.askedCount() != asked {
		t.Fatal("a denial was asked about again within the negative TTL")
	}
	if errs := sw.AwaitRaw(context.Background(), "sb-2"); len(errs) != 0 {
		t.Fatalf("a client with no raw sentinels: errs = %v", errs)
	}

	down := newRawResolver(map[string]RawResult{})
	down.setErr(errors.New("control plane down"))
	sw = New(down, Config{ResolveRetry: 50 * time.Millisecond})
	sw.Reconfigure(Config{ResolveRetry: 50 * time.Millisecond, Raw: map[string][]string{"sb-1": {"RAW-SENTINEL"}}})
	if errs := sw.AwaitRaw(context.Background(), "sb-1"); len(errs) != 1 {
		t.Fatalf("unreachable: errs = %v, want the value unavailable", errs)
	}
	down.setErr(nil)
	down.set("RAW-SENTINEL", RawResult{Value: rawValue}, true)
	if errs := sw.AwaitRaw(context.Background(), "sb-1"); len(errs) != 0 {
		t.Fatalf("once reachable: errs = %v, want none", errs)
	}
	if set := sw.Raw("sb-1"); len(set.Find(rawValue)) != 1 {
		t.Fatal("the awaited value is not watched for")
	}
}
