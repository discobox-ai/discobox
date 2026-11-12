package secrets

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const bodySentinel = "bxd_SENTINEL"

func bodySwapper(value string, err error) *Swapper {
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: value, ExpiresAt: time.Now().Add(time.Hour)}, err
	}}
	return New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {bodySentinel}}})
}

func TestMatchBodyFindsASentinelOnlyAtAPlace(t *testing.T) {
	sw := bodySwapper("v", nil)
	for _, tc := range []struct {
		name   string
		body   string
		places []string
		want   string // "at" of each use, space separated
		err    bool
	}{
		{name: "top-level member", body: `{"api_key":"bxd_SENTINEL"}`, places: []string{"/api_key"}, want: "/api_key"},
		{name: "not a place", body: `{"other":"bxd_SENTINEL"}`, places: []string{"/api_key"}},
		{name: "inside a longer string", body: `{"api_key":"bxd_SENTINEL!"}`, places: []string{"/api_key"}},
		{name: "a member name", body: `{"bxd_SENTINEL":"bxd_SENTINEL"}`, places: []string{"/api_key"}},
		{name: "escaped, not literal", body: `{"api_key":"\u0062xd_SENTINEL","x":"bxd_SENTINEL"}`, places: []string{"/api_key"}},
		{name: "nested and in arrays", body: `{"a":[{"k":1},{"k":"bxd_SENTINEL"}],"b/c":{"d~e":"bxd_SENTINEL"}}`, places: []string{"/a/1/k", "/b~1c/d~0e"}, want: "/a/1/k /b~1c/d~0e"},
		{name: "whitespace around", body: " {\n \"api_key\" :\t\"bxd_SENTINEL\" } ", places: []string{"/api_key"}, want: "/api_key"},
		{name: "a bare string", body: `"bxd_SENTINEL"`, places: []string{"/api_key"}},
		{name: "no sentinel, not parsed", body: `not json at all`, places: []string{"/api_key"}},
		{name: "not JSON", body: `api_key=bxd_SENTINEL`, places: []string{"/api_key"}, err: true},
		{name: "trailing data", body: `{"api_key":"bxd_SENTINEL"} {}`, places: []string{"/api_key"}, err: true},
		{name: "truncated", body: `{"api_key":"bxd_SENTINEL"`, places: []string{"/api_key"}, err: true},
		{name: "a member twice", body: `{"api_key":"x","api_key":"bxd_SENTINEL"}`, places: []string{"/api_key"}, err: true},
		{name: "invalid UTF-8", body: "{\"n\":\"\xff\",\"api_key\":\"bxd_SENTINEL\"}", places: []string{"/api_key"}, err: true},
		{name: "too deep", body: strings.Repeat("[", maxBodyDepth+1) + `"bxd_SENTINEL"` + strings.Repeat("]", maxBodyDepth+1), places: []string{"/0"}, err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uses, err := sw.MatchBody("sandbox-1", []byte(tc.body), tc.places)
			if tc.err {
				if !errors.Is(err, ErrBodyNotJSON) {
					t.Fatalf("MatchBody error = %v, want ErrBodyNotJSON", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, place := range uses.Places() {
				if place.Sentinel != bodySentinel {
					t.Fatalf("found sentinel %q", place.Sentinel)
				}
				got = append(got, place.At)
			}
			if strings.Join(got, " ") != tc.want {
				t.Fatalf("MatchBody found %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMatchBodyIgnoresAnotherClientsSentinels(t *testing.T) {
	uses, err := bodySwapper("v", nil).MatchBody("other", []byte(`{"api_key":"bxd_SENTINEL"}`), []string{"/api_key"})
	if err != nil || !uses.Empty() {
		t.Fatalf("MatchBody for a client without the sentinel = %v, %v", uses.Places(), err)
	}
}

func TestApplySwapsAtBodyPlaces(t *testing.T) {
	const body = `{"api_key": "bxd_SENTINEL", "note": "bxd_SENTINEL"}`
	sw := bodySwapper(`real"<key>`, nil)
	uses, err := sw.MatchBody("sandbox-1", []byte(body), []string{"/api_key"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(context.Background(), "POST", "https://api.x.test/token", strings.NewReader(body))
	res := sw.Apply(context.Background(), req, "sandbox-1", uses)
	if !res.Swapped() || strings.Join(res.BodyPlaces, ",") != "/api_key" || len(res.Sentinels) != 1 {
		t.Fatalf("Apply = %+v, want the body place swapped", res)
	}
	got, _ := io.ReadAll(req.Body)
	const want = `{"api_key": "real\"<key>", "note": "bxd_SENTINEL"}`
	if string(got) != want || req.ContentLength != int64(len(want)) {
		t.Fatalf("body = %q (length %d), want %q", got, req.ContentLength, want)
	}
	if again, _ := req.GetBody(); again == nil {
		t.Fatal("no GetBody for the rewritten body")
	}
}

func TestApplyLeavesADeniedBodyPlace(t *testing.T) {
	const body = `{"api_key":"bxd_SENTINEL"}`
	sw := bodySwapper("", ErrDenied)
	uses, _ := sw.MatchBody("sandbox-1", []byte(body), []string{"/api_key"})
	req := httptest.NewRequestWithContext(context.Background(), "POST", "https://api.x.test/token", strings.NewReader(body))
	if res := sw.Apply(context.Background(), req, "sandbox-1", uses); res.Swapped() || len(res.Errors) > 0 {
		t.Fatalf("Apply of a denied sentinel = %+v, want nothing swapped", res)
	}
	if got, _ := io.ReadAll(req.Body); string(got) != body {
		t.Fatalf("body = %q, want it as sent", got)
	}
}
