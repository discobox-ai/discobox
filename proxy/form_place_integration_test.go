package proxy

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// A credential a route places in a URL-encoded form field — an OAuth token
// endpoint's client_secret — is swapped there: the origin receives the real
// secret as that field's whole value, and the sentinel nowhere.
func TestHTTPProxySwapsASentinelAtAFormField(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk_SENTINEL000000000000000000000000000000"
	const realSecret = "real&secret=with+form/chars%"
	route, err := secretroute.Parse("POST api.x.test/oauth2/get_token body=form:client_secret")
	if err != nil {
		t.Fatalf("a route cannot name a form field: %v", err)
	}
	resolver := &placeResolver{grants: []secretroute.Grant{{Lists: [][]secretroute.Route{{route}}}}, value: realSecret}

	got := make(chan url.Values, 1)
	origin := &routeOrigin{}
	origin.Server = newOrigin(func(_ http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got <- r.PostForm
	})
	t.Cleanup(origin.Close)
	client, _, _ := startRouteProxy(ctx, t, origin, SecretClient{ClientID: "sandbox-1", Sentinels: []string{sentinel}}, resolver)
	port := origin.Listener.Addr().(*net.TCPAddr).Port
	target := (&url.URL{Scheme: "http", Host: net.JoinHostPort("api.x.test", strconv.Itoa(port)), Path: "/oauth2/get_token"}).String()

	form := url.Values{"client_id": {"app"}, "client_secret": {sentinel}, "grant_type": {"authorization_code"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	seen := <-got
	if secret := seen.Get("client_secret"); secret != realSecret {
		t.Fatalf("origin got client_secret %q, want the real secret", secret)
	}
}

// Only the field a form place names is swapped, and only as its whole decoded
// value; everything else in the form reaches the origin byte for byte. A form
// that carries the sentinel and cannot be read for its places as every
// upstream would read it is refused, never sent; a body that is not a form is
// sent unread on a route naming only form places.
func TestHTTPProxyFormPlaceReadsOnlyAFormItCanRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk_SENTINEL000000000000000000000000000000"
	const realSecret = "real secret&x=y"
	route, err := secretroute.Parse("POST api.x.test/oauth2/get_token body=form:client_secret")
	if err != nil {
		t.Fatal(err)
	}
	resolver := &placeResolver{grants: []secretroute.Grant{{Lists: [][]secretroute.Route{{route}}}}, value: realSecret}
	origin := &routeOrigin{}
	origin.Server = newOrigin(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		origin.mu.Lock()
		origin.seen = append(origin.seen, routeOriginRequest{encoding: r.Header.Get("Content-Encoding"), body: body})
		origin.mu.Unlock()
	})
	t.Cleanup(origin.Close)
	client, dbPath, _ := startRouteProxy(ctx, t, origin, SecretClient{ClientID: "sandbox-1", Sentinels: []string{sentinel}}, resolver)
	port := origin.Listener.Addr().(*net.TCPAddr).Port
	target := func(n int) string {
		return (&url.URL{Scheme: "http", Host: net.JoinHostPort("api.x.test", strconv.Itoa(port)), Path: "/oauth2/get_token", RawQuery: "n=" + strconv.Itoa(n)}).String()
	}
	send := func(n int, body io.Reader, length int64, header map[string]string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target(n), body)
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = length
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, string(got)
	}

	// Swapped: the field, written plainly or percent-encoded, under a
	// charset parameter; every other copy of the sentinel is left alone.
	escaped := url.QueryEscape(realSecret)
	for i, tc := range []struct{ sent, want string }{
		{
			sent: "note=" + sentinel + "&client_secret=" + sentinel + "&" + sentinel + "=x&client_secret_2=" + sentinel,
			want: "note=" + sentinel + "&client_secret=" + escaped + "&" + sentinel + "=x&client_secret_2=" + sentinel,
		},
		{
			sent: "client%5Fsecret=" + strings.Replace(sentinel, "_", "%5F", 1) + "&grant_type=authorization_code",
			want: "client%5Fsecret=" + escaped + "&grant_type=authorization_code",
		},
	} {
		before := len(origin.requests())
		status, _ := send(i, strings.NewReader(tc.sent), int64(len(tc.sent)), map[string]string{"Content-Type": "application/x-www-form-urlencoded; charset=UTF-8"})
		if seen := origin.requests(); status != http.StatusOK || len(seen) != before+1 || string(seen[before].body) != tc.want {
			t.Fatalf("%q: status %d, the origin received %q, want %q", tc.sent, status, seen[before:], tc.want)
		}
	}

	// Sent unread: a body that is not a form, or a form that names its
	// field nowhere a sentinel is.
	for i, tc := range []struct {
		body   string
		header map[string]string
	}{
		{body: `{"client_secret":"` + sentinel + `"}`, header: map[string]string{"Content-Type": "application/json"}},
		{body: "client_secret=" + sentinel, header: map[string]string{"Content-Type": "text/plain"}},
		{body: "client_secret=x&client_secret=y;z&note=%zz"},
	} {
		before := len(origin.requests())
		status, _ := send(10+i, strings.NewReader(tc.body), int64(len(tc.body)), tc.header)
		if seen := origin.requests(); status != http.StatusOK || len(seen) != before+1 || string(seen[before].body) != tc.body {
			t.Fatalf("%q: status %d, the origin received %q, want the body as sent", tc.body, status, seen[before:])
		}
	}

	// Refused: a form carrying the sentinel that cannot be read for its
	// place, or read at all. None reaches the origin.
	for i, tc := range []struct {
		name   string
		body   []byte
		length int64
		header map[string]string
		reason string
	}{
		{name: "the field twice", body: []byte("client_secret=" + sentinel + "&client_secret=x"), reason: "not a URL-encoded form"},
		{name: "the field twice, in another case", body: []byte("client_secret=" + sentinel + "&CLIENT_SECRET=x"), reason: "not a URL-encoded form"},
		{name: "the field twice, folded through upper case", body: []byte("client_secret=" + sentinel + "&client_%C5%BFecret=x"), reason: "not a URL-encoded form"},
		{name: "a ; in a pair", body: []byte("client_secret=" + sentinel + ";x=y"), reason: "not a URL-encoded form"},
		{name: "a pair that does not unescape", body: []byte("client_secret=" + sentinel + "&note=%zz"), reason: "not a URL-encoded form"},
		{name: "undeclared length", body: []byte("client_secret=" + sentinel), length: -1, reason: "could not be read"},
		{name: "past the limit", body: []byte("client_secret=" + sentinel + "&pad=" + strings.Repeat("x", routeBodyLimit)), reason: "could not be read"},
		{name: "gzip", body: gzipped(t, "client_secret="+sentinel), header: map[string]string{"Content-Encoding": "gzip"}, reason: "without a Content-Encoding"},
	} {
		before := len(origin.requests())
		length := tc.length
		if length == 0 {
			length = int64(len(tc.body))
		}
		var body io.Reader = bytes.NewReader(tc.body)
		if length < 0 {
			body = io.MultiReader(body)
		}
		status, got := send(20+i, body, length, tc.header)
		if status != http.StatusForbidden || !strings.Contains(got, tc.reason) {
			t.Fatalf("%s: response = %d %q, want a 403 saying %q", tc.name, status, got, tc.reason)
		}
		if len(origin.requests()) != before {
			t.Fatalf("%s: a refused request reached the origin", tc.name)
		}
	}
	if rows := waitForAuditRows(t, dbPath, true, 8); len(rows) != 8 {
		t.Fatalf("recorded %d refusals, want 8", len(rows))
	}
}
