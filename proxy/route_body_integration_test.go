package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/proxy/internal/secrets"
)

// routeResolver authorizes a request the way the pool's resolver does: held
// to route lists by secretroute.Decide, over the body the proxy hands it. It
// hands out the next of values on every resolve, so a retry has a new one.
type routeResolver struct {
	noGatewayHeaders
	reportLog
	lists [][]secretroute.Route

	mu     sync.Mutex
	values []string
	calls  int
	// bodies is what each body the authorizer read came to: the decoded body,
	// or "<unreadable>".
	bodies []string
}

func (r *routeResolver) Gate(context.Context, secrets.GateRequest) (secrets.GateAdmission, error) {
	return secrets.GateAdmission{}, &secrets.GateRefusal{Reason: "no gate here"}
}

func (r *routeResolver) Authorize(_ context.Context, req secrets.AuthorizeRequest) (secrets.Verdict, error) {
	read := req.Body
	body := func() ([]byte, bool) {
		if read == nil {
			return nil, false
		}
		decoded, ok := read()
		r.mu.Lock()
		defer r.mu.Unlock()
		if ok {
			r.bodies = append(r.bodies, string(decoded))
		} else {
			r.bodies = append(r.bodies, "<unreadable>")
		}
		return decoded, ok
	}
	r.mu.Lock()
	lists := r.lists
	r.mu.Unlock()
	allowed, reason := secretroute.Decide(lists, secretroute.Request{Method: req.Method, Host: req.Host, URL: req.URL, Header: req.Header, Body: body})
	return secrets.Verdict{Allow: allowed, Reason: reason}, nil
}

func (r *routeResolver) Resolve(context.Context, secrets.ResolveRequest) (secrets.ResolveResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value := r.values[min(r.calls, len(r.values)-1)]
	r.calls++
	return secrets.ResolveResult{Value: value, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (r *routeResolver) readBodies() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bodies...)
}

// routeOrigin is an upstream that keeps every request it receives, and
// answers 401 to any credential but accept.
type routeOrigin struct {
	*httptest.Server
	mu   sync.Mutex
	seen []routeOriginRequest
}

type routeOriginRequest struct {
	authorization, encoding string
	body                    []byte
}

func newRouteOrigin(t *testing.T, accept string) *routeOrigin {
	t.Helper()
	o := &routeOrigin{}
	o.Server = newOrigin(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		o.mu.Lock()
		o.seen = append(o.seen, routeOriginRequest{authorization: r.Header.Get("Authorization"), encoding: r.Header.Get("Content-Encoding"), body: body})
		o.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+accept {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	t.Cleanup(o.Close)
	return o
}

func (o *routeOrigin) requests() []routeOriginRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]routeOriginRequest(nil), o.seen...)
}

// startRouteProxy brings up a proxy recording bodies, whose upstream transport
// sends every dial to origin, so a request to any name reaches it.
func startRouteProxy(ctx context.Context, t *testing.T, origin *routeOrigin, sentinel string, resolver SecretResolver) (client *http.Client, dbPath, bodyDir string) {
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
	dbPath = filepath.Join(dir, "audit.db")
	bodyDir = filepath.Join(dir, "bodies")
	server, err := NewServer(ctx, Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   dbPath,
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16, BodyDir: bodyDir},
		Secrets:       SecretsConfig{Clients: []SecretClient{{ClientID: "sandbox-1", Sentinels: []string{sentinel}}}},
	}, prepared.Bundle, resolver)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	originAddr := origin.Listener.Addr().String()
	var dialer net.Dialer
	server.http.proxy.Tr = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, originAddr)
		},
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	t.Cleanup(closeProxyServer(t, server, errCh))
	addr := waitForAddr(t, server)
	return mtlsHTTPClient(t, addr.String(), prepared.Clients["sandbox-1"]), dbPath, bodyDir
}

func gzipped(t *testing.T, text string) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	if _, err := io.WriteString(writer, text); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// A route to GraphQL queries is held to the body the request sends: a query
// is sent and a mutation refused, compressed or not. The upstream receives the
// body exactly as the sandbox sent it, and the audit spool records all of it,
// though the authorizer read it first. A body the authorizer cannot read —
// past the limit, or in an encoding it does not decode — matches no route
// with a protocol block, and is refused.
func TestHTTPProxyHoldsAGraphQLRouteToTheRequestBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-SENTINELVALUE000000000000000000000000000"
	const realValue = "sk-REALSECRETVALUE0000000000000000000000000"
	route, err := secretroute.Parse("api.x.test/graphql graphql=query")
	if err != nil {
		t.Fatal(err)
	}
	resolver := &routeResolver{lists: [][]secretroute.Route{{route}}, values: []string{realValue}}
	origin := newRouteOrigin(t, realValue)
	client, dbPath, bodyDir := startRouteProxy(ctx, t, origin, sentinel, resolver)
	port := origin.Listener.Addr().(*net.TCPAddr).Port

	const query = `{"query":"query { viewer { login } }"}`
	const mutation = `{"query":"mutation { deleteRepository(input: {repositoryId: \"r\"}) { clientMutationId } }"}`
	oversized := `{"query":"query { viewer { login } }","pad":"` + strings.Repeat("x", routeBodyLimit) + `"}`

	for i, tc := range []struct {
		name     string
		body     []byte
		encoding string
		decoded  string
		allowed  bool
	}{
		{name: "a query", body: []byte(query), decoded: query, allowed: true},
		{name: "a mutation", body: []byte(mutation), decoded: mutation},
		{name: "a gzip query", body: gzipped(t, query), encoding: "gzip", decoded: query, allowed: true},
		{name: "a gzip mutation", body: gzipped(t, mutation), encoding: "gzip", decoded: mutation},
		{name: "a query past the limit", body: []byte(oversized), decoded: "<unreadable>"},
		{name: "a gzip query decoding past the limit", body: gzipped(t, oversized), encoding: "gzip", decoded: "<unreadable>"},
		{name: "a query in an encoding not decoded", body: []byte(query), encoding: "br", decoded: "<unreadable>"},
	} {
		// Each request has its own URL, so its audit row can be told apart.
		target := (&url.URL{Scheme: "http", Host: net.JoinHostPort("api.x.test", strconv.Itoa(port)), Path: "/graphql", RawQuery: "n=" + strconv.Itoa(i)}).String()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+sentinel)
		req.Header.Set("Content-Type", "application/json")
		if tc.encoding != "" {
			req.Header.Set("Content-Encoding", tc.encoding)
		}
		before := len(origin.requests())
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: client.Do() error = %v", tc.name, err)
		}
		got, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if bodies := resolver.readBodies(); len(bodies) != i+1 || bodies[i] != tc.decoded {
			t.Fatalf("%s: the authorizer read %d bodies, the last %.80q; want it to read %.80q", tc.name, len(bodies), bodies[len(bodies)-1], tc.decoded)
		}
		seen := origin.requests()
		if !tc.allowed {
			if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(got), "route") {
				t.Fatalf("%s: response = %d %q, want a 403 naming the route", tc.name, resp.StatusCode, got)
			}
			if len(seen) != before {
				t.Fatalf("%s: a refused request reached the upstream", tc.name)
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: response = %d %q, want 200", tc.name, resp.StatusCode, got)
		}
		if len(seen) != before+1 {
			t.Fatalf("%s: the upstream received %d requests, want 1", tc.name, len(seen)-before)
		}
		sent := seen[len(seen)-1]
		if !bytes.Equal(sent.body, tc.body) || sent.encoding != tc.encoding || sent.authorization != "Bearer "+realValue {
			t.Fatalf("%s: the upstream received %q (encoding %q, Authorization %q), want the body as sent, its encoding, and the credential",
				tc.name, sent.body, sent.encoding, sent.authorization)
		}
		exchange := waitForHTTPExchange(t, dbPath, "client_id = ? AND url = ?", "sandbox-1", target)
		if exchange.RequestBodyBytes != int64(len(tc.body)) {
			t.Fatalf("%s: the audit recorded %d request body bytes, want %d", tc.name, exchange.RequestBodyBytes, len(tc.body))
		}
		assertSpoolFile(t, filepath.Join(bodyDir, filepath.FromSlash(exchange.RequestBodyFile)), string(tc.body))
	}
}

// A retry carries a credential the first attempt did not, so it is held to
// the routes again, over the body it resends: the same body, read from the
// retry's buffer, and sent to the upstream unchanged both times.
func TestHTTPProxyHoldsARetryToAGraphQLRoute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-SENTINELVALUE000000000000000000000000000"
	const stale = "sk-STALEVALUE000000000000000000000000000000"
	const rotated = "sk-ROTATEDVALUE00000000000000000000000000000"
	route, err := secretroute.Parse("api.x.test/graphql graphql=query")
	if err != nil {
		t.Fatal(err)
	}
	resolver := &routeResolver{lists: [][]secretroute.Route{{route}}, values: []string{stale, rotated}}
	origin := newRouteOrigin(t, rotated)
	client, _, _ := startRouteProxy(ctx, t, origin, sentinel, resolver)
	port := origin.Listener.Addr().(*net.TCPAddr).Port

	const query = `{"query":"query { viewer { login } }"}`
	body := gzipped(t, query)
	target := "http://" + net.JoinHostPort("api.x.test", strconv.Itoa(port)) + "/graphql"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sentinel)
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("response = %d %q, want the retry's 200", resp.StatusCode, got)
	}
	if bodies := resolver.readBodies(); len(bodies) != 2 || bodies[0] != query || bodies[1] != query {
		t.Fatalf("the authorizer read %q, want the decoded query on the first attempt and on the retry", bodies)
	}
	seen := origin.requests()
	if len(seen) != 2 {
		t.Fatalf("the upstream received %d requests, want the first attempt and the retry", len(seen))
	}
	for i, sent := range seen {
		if !bytes.Equal(sent.body, body) || sent.encoding != "gzip" {
			t.Fatalf("attempt %d reached the upstream as %q (encoding %q), want the body as sent", i, sent.body, sent.encoding)
		}
	}
	if seen[0].authorization != "Bearer "+stale || seen[1].authorization != "Bearer "+rotated {
		t.Fatalf("attempts carried %q then %q, want the stale credential then the rotated one", seen[0].authorization, seen[1].authorization)
	}
}

// A retry whose body is a mutation is refused like its first attempt — here
// the routes changed under it, so the first attempt was allowed — and the
// upstream's own 401 stands: nothing is resent.
func TestHTTPProxyRefusesARetryItsRoutesNoLongerAllow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-SENTINELVALUE000000000000000000000000000"
	const stale = "sk-STALEVALUE000000000000000000000000000000"
	const rotated = "sk-ROTATEDVALUE00000000000000000000000000000"
	both, err := secretroute.Parse("api.x.test/graphql graphql=query,mutation")
	if err != nil {
		t.Fatal(err)
	}
	queries, err := secretroute.Parse("api.x.test/graphql graphql=query")
	if err != nil {
		t.Fatal(err)
	}
	resolver := &routeResolver{lists: [][]secretroute.Route{{both}}, values: []string{stale, rotated}}
	narrowing := &narrowingResolver{routeResolver: resolver, after: [][]secretroute.Route{{queries}}}
	origin := newRouteOrigin(t, rotated)
	client, _, _ := startRouteProxy(ctx, t, origin, sentinel, narrowing)
	port := origin.Listener.Addr().(*net.TCPAddr).Port

	const mutation = `{"query":"mutation { addComment(input: {}) { clientMutationId } }"}`
	target := "http://" + net.JoinHostPort("api.x.test", strconv.Itoa(port)) + "/graphql"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(mutation))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sentinel)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("response = %d, want the upstream's own 401", resp.StatusCode)
	}
	if bodies := resolver.readBodies(); len(bodies) != 2 || bodies[1] != mutation {
		t.Fatalf("the authorizer read %q, want the mutation read again for the retry", bodies)
	}
	if seen := origin.requests(); len(seen) != 1 {
		t.Fatalf("the upstream received %d requests, want only the first attempt", len(seen))
	}
}

// narrowingResolver holds the first request to routeResolver's lists and
// every one after it to after.
type narrowingResolver struct {
	*routeResolver
	once  sync.Once
	after [][]secretroute.Route
}

func (r *narrowingResolver) Authorize(ctx context.Context, req secrets.AuthorizeRequest) (secrets.Verdict, error) {
	verdict, err := r.routeResolver.Authorize(ctx, req)
	r.once.Do(func() {
		r.mu.Lock()
		r.lists = r.after
		r.mu.Unlock()
	})
	return verdict, err
}

// A method named in a header is a method the request asks for: a POST that
// says X-HTTP-Method-Override: DELETE is held to the route blocking DELETE,
// refused, and never sent, while a plain POST on the same path is sent.
func TestHTTPProxyHoldsAMethodOverrideToItsRoutes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "sk-SENTINELVALUE000000000000000000000000000"
	const realValue = "sk-REALSECRETVALUE0000000000000000000000000"
	var lists []secretroute.Route
	for _, text := range []string{"api.x.test/**", "DELETE api.x.test/repos/a=blocked"} {
		route, err := secretroute.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		lists = append(lists, route)
	}
	resolver := &routeResolver{lists: [][]secretroute.Route{lists}, values: []string{realValue}}
	origin := newRouteOrigin(t, realValue)
	client, _, _ := startRouteProxy(ctx, t, origin, sentinel, resolver)
	target := "http://" + net.JoinHostPort("api.x.test", strconv.Itoa(origin.Listener.Addr().(*net.TCPAddr).Port)) + "/repos/a"

	post := func(override string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+sentinel)
		if override != "" {
			req.Header.Set("X-HTTP-Method-Override", override)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("client.Do() error = %v", err)
		}
		got, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, string(got)
	}

	if status, got := post("DELETE"); status != http.StatusForbidden || !strings.Contains(got, "route") {
		t.Fatalf("an overridden DELETE = %d %q, want a 403 naming the route", status, got)
	}
	if seen := origin.requests(); len(seen) != 0 {
		t.Fatalf("a refused override reached the upstream %d times", len(seen))
	}
	if status, got := post(""); status != http.StatusOK {
		t.Fatalf("a plain POST = %d %q, want 200", status, got)
	}
	if seen := origin.requests(); len(seen) != 1 || seen[0].authorization != "Bearer "+realValue {
		t.Fatalf("the upstream received %+v, want the plain POST with the credential", seen)
	}
}
