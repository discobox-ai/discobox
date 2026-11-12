package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/proxy/internal/secrets"
)

// placeResolver authorizes and names body places the way the pool's resolver
// does (pool-agent/proxyagent), over one grant of route lists.
type placeResolver struct {
	noGatewayHeaders
	reportLog
	grants []secretroute.Grant
	value  string

	mu       sync.Mutex
	resolves int
}

func (r *placeResolver) ResolveRaw(context.Context, secrets.RawRequest) (secrets.RawResult, error) {
	return secrets.RawResult{}, secrets.ErrDenied
}

func (r *placeResolver) Gate(context.Context, secrets.GateRequest) (secrets.GateAdmission, error) {
	return secrets.GateAdmission{}, &secrets.GateRefusal{Reason: "no gate here"}
}

func (r *placeResolver) Authorize(_ context.Context, req secrets.AuthorizeRequest) (secrets.Verdict, error) {
	uses := map[string][]string{}
	for _, sentinel := range req.Sentinels {
		uses[sentinel] = append(uses[sentinel], secretroute.InHeaders)
	}
	for _, place := range req.Places {
		uses[place.Sentinel] = append(uses[place.Sentinel], place.At)
	}
	for _, at := range uses {
		if allowed, reason := secretroute.DecideGrants(r.grants, time.Now(), secretroute.Request{
			Method: req.Method, Host: req.Host, URL: req.URL, Header: req.Header, Body: req.Body, At: at,
		}); !allowed {
			return secrets.Verdict{Reason: reason}, nil
		}
	}
	return secrets.Verdict{Allow: true}, nil
}

func (r *placeResolver) BodyPlaces(_ context.Context, req secrets.BodyPlacesRequest) []string {
	return secretroute.BodyPlaces(r.grants, time.Now(), secretroute.Request{Method: req.Method, Host: req.Host, URL: req.URL})
}

func (r *placeResolver) Resolve(context.Context, secrets.ResolveRequest) (secrets.ResolveResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolves++
	return secrets.ResolveResult{Value: r.value, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// A credential a route places in a JSON request body is swapped there and
// nowhere else: the origin receives the real key at /api_key, and every other
// copy of the sentinel — another field, a longer string, a member name —
// exactly as the sandbox sent it. The trail keeps the body as sent, with the
// sentinel, and nothing of the answer: neither the token in its body nor in a
// header. A request whose body places cannot be read for a sentinel is
// refused and recorded as blocked, never sent.
func TestHTTPProxySwapsASentinelAtABodyPlace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "bxd_SENTINEL000000000000000000000000000000"
	const realKey = "bxd_REALKEY\"quoted\\0000000000000000000000"
	const token = "eyJREALJWT.issued.for-an-hour" //nolint:gosec // a fake token, for the test origin to issue
	route, err := secretroute.Parse("POST api.x.test/api/v1/auth/token body=/api_key")
	if err != nil {
		t.Fatal(err)
	}
	resolver := &placeResolver{grants: []secretroute.Grant{{Lists: [][]secretroute.Route{{route}}}}, value: realKey}

	origin := &routeOrigin{}
	origin.Server = newOrigin(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		origin.mu.Lock()
		origin.seen = append(origin.seen, routeOriginRequest{encoding: r.Header.Get("Content-Encoding"), body: body})
		origin.mu.Unlock()
		var exchange struct {
			APIKey string `json:"api_key"`
		}
		if json.Unmarshal(body, &exchange) != nil || exchange.APIKey != realKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-Session-Token", token)
		w.Header().Set("Location", "/session/"+token)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"`+token+`"}`)
	})
	t.Cleanup(origin.Close)
	client, dbPath, bodyDir := startRouteProxy(ctx, t, origin, sentinel, resolver)
	port := origin.Listener.Addr().(*net.TCPAddr).Port
	target := func(path string, n int) string {
		return (&url.URL{Scheme: "http", Host: net.JoinHostPort("api.x.test", strconv.Itoa(port)), Path: path, RawQuery: "n=" + strconv.Itoa(n)}).String()
	}
	send := func(method, target string, body io.Reader, length int64, header map[string]string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, target, body)
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = length
		req.Header.Set("Content-Type", "application/json")
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("client.Do() error = %v", err)
		}
		got, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, string(got)
	}

	// The exchange: one place swapped, every other copy left alone.
	sent := `{"api_key": "` + sentinel + `", "note":"` + sentinel + ` and more", "` + sentinel + `":"x", "nested":{"api_key":"` + sentinel + `"}}`
	exchangeURL := target("/api/v1/auth/token", 0)
	status, got := send(http.MethodPost, exchangeURL, strings.NewReader(sent), int64(len(sent)), nil)
	if status != http.StatusOK || !strings.Contains(got, token) {
		t.Fatalf("token exchange = %d %q, want 200 with the token", status, got)
	}
	keyJSON, err := json.Marshal(realKey)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(sent, `"`+sentinel+`"`, string(keyJSON), 1)
	if seen := origin.requests(); len(seen) != 1 || string(seen[0].body) != want {
		t.Fatalf("the origin received %q, want %q", origin.requests(), want)
	}
	exchange := waitForHTTPExchange(t, dbPath, "client_id = ? AND url = ?", "sandbox-1", exchangeURL)
	assertSpoolFile(t, filepath.Join(bodyDir, filepath.FromSlash(exchange.RequestBodyFile)), sent)
	if exchange.ResponseBodyFile != "" || exchange.ResponseBodyFormat != "" {
		t.Fatalf("the answer to a body swap was spooled: %q/%q", exchange.ResponseBodyFormat, exchange.ResponseBodyFile)
	}
	if strings.Contains(exchange.ResponseHeaders, token) || strings.Contains(exchange.ResponseHeaders, "/session/") ||
		!strings.Contains(exchange.ResponseHeaders, "application/json") {
		t.Fatalf("the audit kept %s, want every response header value redacted but the framing ones", exchange.ResponseHeaders)
	}

	// Elsewhere, or by another method, the body is not read and nothing in
	// it is swapped.
	elsewhere := `{"api_key":"` + sentinel + `"}`
	for i, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/other"},
		{http.MethodPut, "/api/v1/auth/token"},
	} {
		before := len(origin.requests())
		send(tc.method, target(tc.path, 10+i), strings.NewReader(elsewhere), int64(len(elsewhere)), nil)
		if seen := origin.requests(); len(seen) != before+1 || string(seen[before].body) != elsewhere {
			t.Fatalf("%s %s: the origin received %q, want the body as sent", tc.method, tc.path, seen[before:])
		}
	}

	// A body that does not say it is JSON — an upload, a form — is sent
	// unread on the same route, however long or chunked: a place is in a
	// JSON body.
	for i, contentType := range []string{"application/octet-stream", "application/x-www-form-urlencoded"} {
		before := len(origin.requests())
		upload := `{"api_key":"` + sentinel + `"}` + strings.Repeat("x", routeBodyLimit)
		send(http.MethodPost, target("/api/v1/auth/token", 40+i), io.MultiReader(strings.NewReader(upload)), -1, map[string]string{"Content-Type": contentType})
		if seen := origin.requests(); len(seen) != before+1 || string(seen[before].body) != upload {
			t.Fatalf("%s: the origin did not receive the body as sent", contentType)
		}
	}

	// Refused: a body that cannot be read, or read for a place, and a
	// request its route refuses. None reaches the origin.
	for i, tc := range []struct {
		name   string
		url    string
		body   []byte
		length int64
		header map[string]string
		reason string
	}{
		{name: "undeclared length", body: []byte(elsewhere), length: -1, reason: "could not be read"},
		{name: "past the limit", body: []byte(`{"api_key":"` + sentinel + `","pad":"` + strings.Repeat("x", routeBodyLimit) + `"}`), reason: "could not be read"},
		{name: "gzip", body: gzipped(t, elsewhere), header: map[string]string{"Content-Encoding": "gzip"}, reason: "without a Content-Encoding"},
		{name: "not JSON", body: []byte("api_key=" + sentinel), reason: "not JSON"},
		{name: "a member twice", body: []byte(`{"api_key":"` + sentinel + `","api_key":"x"}`), reason: "not JSON"},
		{name: "a method override", url: target("/api/v1/auth/token", 30) + "&_method=DELETE", body: []byte(elsewhere), reason: "does not allow DELETE"},
	} {
		before := len(origin.requests())
		u := tc.url
		if u == "" {
			u = target("/api/v1/auth/token", 20+i)
		}
		length := tc.length
		if length == 0 {
			length = int64(len(tc.body))
		}
		var body io.Reader = bytes.NewReader(tc.body)
		if length < 0 {
			body = io.MultiReader(body) // hides its length from the client
		}
		status, got := send(http.MethodPost, u, body, length, tc.header)
		if status != http.StatusForbidden || !strings.Contains(got, tc.reason) {
			t.Fatalf("%s: response = %d %q, want a 403 saying %q", tc.name, status, got, tc.reason)
		}
		if len(origin.requests()) != before {
			t.Fatalf("%s: a refused request reached the origin", tc.name)
		}
	}
	blocked := waitForBlockedRows(t, dbPath, 6)
	for _, row := range blocked {
		if !strings.HasPrefix(row.BlockedReason, "route: ") {
			t.Fatalf("blocked row reason = %q, want a route refusal", row.BlockedReason)
		}
	}

	// Nothing the proxy wrote holds the key or the token.
	root, err := os.OpenRoot(filepath.Dir(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := root.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte("REALKEY")) || bytes.Contains(data, []byte("REALJWT")) {
			t.Fatalf("%s holds the key or the token", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
