package secrets

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"
)

func basicAuth(userinfo string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(userinfo))
}

func encodingSwapper(t *testing.T, sentinels ...string) (*Swapper, *fakeResolver) {
	t.Helper()
	resolver := &fakeResolver{fn: func(req ResolveRequest) (ResolveResult, error) {
		switch req.Sentinel {
		case "ghp_SENTINELSENTINELSENTINELSENTINEL":
			return ResolveResult{Value: "ghp_realrealrealrealrealrealrealreal", ExpiresAt: time.Now().Add(time.Hour)}, nil
		case "SENTINELUSERNAME":
			return ResolveResult{Value: "real-username", ExpiresAt: time.Now().Add(time.Hour)}, nil
		}
		return ResolveResult{}, ErrDenied
	}}
	return New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": sentinels}}), resolver
}

// A sentinel that Git base64-encodes as the password half of basic auth is the
// case that started this: a literal scan sees nothing and GitHub is handed the
// placeholder.
func TestSwapBase64BasicAuthPassword(t *testing.T) {
	sw, _ := encodingSwapper(t, "ghp_SENTINELSENTINELSENTINELSENTINEL")

	req := newRequest(t, http.MethodGet, "https://github.com/org/repo.git/info/refs")
	req.Header.Set("Authorization", basicAuth("x-access-token:ghp_SENTINELSENTINELSENTINELSENTINEL"))

	res := sw.Apply(context.Background(), req, "sandbox-1")
	if !res.Swapped() {
		t.Fatal("expected swap")
	}
	if !res.Encoded {
		t.Fatal("Encoded = false, want true for a swap inside a base64 token")
	}
	if got, want := req.Header.Get("Authorization"), basicAuth("x-access-token:ghp_realrealrealrealrealrealrealreal"); got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
	if len(res.Headers) != 1 || res.Headers[0] != "Authorization" {
		t.Fatalf("Headers = %v, want [Authorization]", res.Headers)
	}
}

func TestSwapBase64BasicAuthUsernameAndBoth(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sentinels []string
		userinfo  string
		want      string
	}{
		{
			name:      "username half",
			sentinels: []string{"SENTINELUSERNAME"},
			userinfo:  "SENTINELUSERNAME:x-oauth-basic",
			want:      "real-username:x-oauth-basic",
		},
		{
			name:      "both halves",
			sentinels: []string{"SENTINELUSERNAME", "ghp_SENTINELSENTINELSENTINELSENTINEL"},
			userinfo:  "SENTINELUSERNAME:ghp_SENTINELSENTINELSENTINELSENTINEL",
			want:      "real-username:ghp_realrealrealrealrealrealrealreal",
		},
		{
			name:      "whole decoded string",
			sentinels: []string{"ghp_SENTINELSENTINELSENTINELSENTINEL"},
			userinfo:  "ghp_SENTINELSENTINELSENTINELSENTINEL",
			want:      "ghp_realrealrealrealrealrealrealreal",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sw, _ := encodingSwapper(t, tc.sentinels...)
			req := newRequest(t, http.MethodGet, "https://github.com/org/repo.git/info/refs")
			req.Header.Set("Authorization", basicAuth(tc.userinfo))

			if res := sw.Apply(context.Background(), req, "sandbox-1"); !res.Swapped() {
				t.Fatal("expected swap")
			}
			if got, want := req.Header.Get("Authorization"), basicAuth(tc.want); got != want {
				t.Fatalf("Authorization = %q, want %q", got, want)
			}
		})
	}
}

// The token is re-encoded the way it arrived: an unpadded or URL-alphabet token
// must not come back padded or in the standard alphabet.
func TestSwapBase64PreservesEncoding(t *testing.T) {
	const sentinel = "ghp_SENTINELSENTINELSENTINELSENTINEL"
	const realValue = "ghp_realrealrealrealrealrealrealreal"
	// "?" forces a byte that differs between the two alphabets once encoded.
	payload := "user:" + sentinel + "\xfb\xff"
	want := "user:" + realValue + "\xfb\xff"

	for _, tc := range []struct {
		name     string
		encoding *base64.Encoding
	}{
		{"std padded", base64.StdEncoding},
		{"url padded", base64.URLEncoding},
		{"std raw", base64.RawStdEncoding},
		{"url raw", base64.RawURLEncoding},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sw, _ := encodingSwapper(t, sentinel)
			req := newRequest(t, http.MethodGet, "https://github.com/org/repo.git/info/refs")
			req.Header.Set("Authorization", "Basic "+tc.encoding.EncodeToString([]byte(payload)))

			if res := sw.Apply(context.Background(), req, "sandbox-1"); !res.Swapped() {
				t.Fatal("expected swap")
			}
			got := req.Header.Get("Authorization")
			if want := "Basic " + tc.encoding.EncodeToString([]byte(want)); got != want {
				t.Fatalf("Authorization = %q, want %q", got, want)
			}
		})
	}
}

func TestSwapBase64QueryParam(t *testing.T) {
	const sentinel = "ghp_SENTINELSENTINELSENTINELSENTINEL"
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: "REAL", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	sw := New(resolver, Config{
		Sentinels: map[string][]string{"sandbox-1": {sentinel}},
		ScanQuery: true,
	})

	encoded := base64.StdEncoding.EncodeToString([]byte("user:" + sentinel))
	req := newRequest(t, http.MethodGet, "https://example.com/api?auth="+encoded)
	if res := sw.Apply(context.Background(), req, "sandbox-1"); !res.Swapped() {
		t.Fatal("expected swap")
	}
	if got, want := req.URL.Query().Get("auth"), base64.StdEncoding.EncodeToString([]byte("user:REAL")); got != want {
		t.Fatalf("auth = %q, want %q", got, want)
	}
}

// Anything that is not an encoded sentinel comes back byte-for-byte, including
// base64 that decodes to something else and base64 the proxy cannot resolve.
func TestSwapBase64LeavesUnrelatedValuesAlone(t *testing.T) {
	const sentinel = "ghp_SENTINELSENTINELSENTINELSENTINEL"
	sw, resolver := encodingSwapper(t, sentinel)

	unrelated := basicAuth("someuser:somepassword")
	req := newRequest(t, http.MethodGet, "https://github.com/org/repo.git/info/refs")
	req.Header.Set("Authorization", unrelated)
	req.Header.Set("X-Blob", "AAAAAAAAAAAAAAAAAAAAAAAA")
	req.Header.Set("X-Not-Base64", "this is not=base64 at all!!")
	req.Header.Set("Cookie", "session=bm90LWEtc2VudGluZWwtYXQtYWxs; other=1")

	if res := sw.Apply(context.Background(), req, "sandbox-1"); res.Swapped() {
		t.Fatalf("unexpected swap: %+v", res)
	}
	if got := req.Header.Get("Authorization"); got != unrelated {
		t.Fatalf("Authorization = %q, want unchanged %q", got, unrelated)
	}
	if got := req.Header.Get("Cookie"); got != "session=bm90LWEtc2VudGluZWwtYXQtYWxs; other=1" {
		t.Fatalf("Cookie = %q, want unchanged", got)
	}
	if resolver.calls.Load() != 0 {
		t.Fatalf("resolver called %d times, want 0", resolver.calls.Load())
	}
}

// A denied sentinel inside a base64 token leaves the token exactly as it
// arrived, so the upstream receives the placeholder and rejects it.
func TestSwapBase64DeniedLeavesTokenIntact(t *testing.T) {
	sw, _ := encodingSwapper(t, "SENTINELDENIED")

	original := basicAuth("x-access-token:SENTINELDENIED")
	req := newRequest(t, http.MethodGet, "https://evil.example.com/")
	req.Header.Set("Authorization", original)

	if res := sw.Apply(context.Background(), req, "sandbox-1"); res.Swapped() {
		t.Fatal("expected no swap on denial")
	}
	if got := req.Header.Get("Authorization"); got != original {
		t.Fatalf("Authorization = %q, want unchanged %q", got, original)
	}
}

// The retry path scans the same way, so a rejected base64-encoded credential
// can fall back to the value the last rotation displaced.
func TestApplyPreviousSwapsBase64(t *testing.T) {
	const sentinel = "ghp_SENTINELSENTINELSENTINELSENTINEL"
	now := time.Unix(1000, 0)
	value := "ghp_first"
	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: value, ExpiresAt: now.Add(30 * time.Second)}, nil
	}}
	sw := New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {sentinel}}})
	sw.now = func() time.Time { return now }

	apply := func() string {
		req := newRequest(t, http.MethodGet, "https://github.com/org/repo.git/info/refs")
		req.Header.Set("Authorization", basicAuth("x-access-token:"+sentinel))
		sw.Apply(context.Background(), req, "sandbox-1")
		return req.Header.Get("Authorization")
	}
	apply()

	// Rotation: past the hard expiry, the next use resolves and displaces.
	now = now.Add(time.Minute)
	value = "ghp_second"
	if got, want := apply(), basicAuth("x-access-token:ghp_second"); got != want {
		t.Fatalf("after rotation Authorization = %q, want %q", got, want)
	}

	retry := newRequest(t, http.MethodGet, "https://github.com/org/repo.git/info/refs")
	retry.Header.Set("Authorization", basicAuth("x-access-token:"+sentinel))
	if res := sw.ApplyPrevious(retry, "sandbox-1"); !res.Swapped() {
		t.Fatal("expected previous-value swap")
	}
	if got, want := retry.Header.Get("Authorization"), basicAuth("x-access-token:ghp_first"); got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

// The encoding a token arrived in is only partly observable. These pin which
// half is read off the token and which is a default, so a change to either is
// deliberate rather than incidental.
func TestTokenEncodingReadsWhatIsObservable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
		want  *base64.Encoding
	}{
		// Determined: the alphabet is visible in the characters used.
		{"url alphabet, padded", "-_8=aaaa", base64.URLEncoding},
		{"std alphabet, padded", "+/8=aaaa", base64.StdEncoding},
		// Determined: a length that is not a multiple of 4 cannot be padded.
		{"url alphabet, raw", "-_8aaaa", base64.RawURLEncoding},
		{"std alphabet, raw", "+/8aaaa", base64.RawStdEncoding},
		// Ambiguous on both axes: no character 62/63, and a length that a padded
		// encoder and a raw encoder both produce. Resolves to standard padded,
		// which is what `Authorization: Basic` is defined to carry.
		{"ambiguous", "dXNlcjpwYXNz", base64.StdEncoding},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tokenEncoding(tc.token); got != tc.want {
				t.Fatalf("tokenEncoding(%q) = %v, want %v", tc.token, got, tc.want)
			}
		})
	}
}

// Whatever the encoding resolves to, it must re-encode the token it came from
// to itself. This is what keeps an unrewritten token byte-identical, and it
// holds for the ambiguous case too.
func TestTokenEncodingRoundTripsTheTokenItCameFrom(t *testing.T) {
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding,
	} {
		// Lengths 0..8 cover every padding remainder; the byte forces the
		// alphabets apart.
		for size := range 9 {
			payload := append([]byte{0xfb, 0xff}, make([]byte, size)...)
			token := encoding.EncodeToString(payload)
			picked := tokenEncoding(token)
			decoded, err := picked.Strict().DecodeString(token)
			if err != nil {
				t.Fatalf("token %q from %v did not decode under %v: %v", token, encoding, picked, err)
			}
			if got := picked.EncodeToString(decoded); got != token {
				t.Fatalf("token %q from %v re-encoded to %q", token, encoding, got)
			}
		}
	}
}

// A Git basic-auth token is the ambiguous case in practice: `x-access-token:`
// plus the sentinel lands on a 3-byte boundary, so the token carries no padding
// to read the encoding off. The real credential is a different length, so the
// rewritten token picks up padding — which is correct for RFC 7617, and is the
// one place the default is doing real work.
func TestSwapBase64PaddingWhenTheTokenCarriedNone(t *testing.T) {
	const sentinel = "ghp_SENTINELVALUE0000000000000000000000"
	original := basicAuth("x-access-token:" + sentinel)
	if strings.Contains(original, "=") {
		t.Fatalf("test premise broken: %q already carries padding", original)
	}

	resolver := &fakeResolver{fn: func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: "ghp_REAL", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	sw := New(resolver, Config{Sentinels: map[string][]string{"sandbox-1": {sentinel}}})

	req := newRequest(t, http.MethodGet, "https://github.com/org/repo.git/info/refs")
	req.Header.Set("Authorization", original)
	if res := sw.Apply(context.Background(), req, "sandbox-1"); !res.Swapped() {
		t.Fatal("expected swap")
	}

	got := req.Header.Get("Authorization")
	if want := basicAuth("x-access-token:ghp_REAL"); got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, "=") {
		t.Fatalf("Authorization = %q, want the standard padded spelling RFC 7617 requires", got)
	}
}
