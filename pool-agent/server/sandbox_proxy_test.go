package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

func TestSandboxAgentProxyRewritesToSandboxAgentAndForwardsDownstreamToken(t *testing.T) {
	projectID := "project-1"
	poolID := "pool-1"
	sandboxID := "sandbox-1"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/projects/project-1/sandboxes/sandbox-1/execs" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sandbox-token" {
			t.Fatalf("upstream authorization = %q", got)
		}
		if got := r.Header.Get(sandboxAgentAuthorizationHeader); got != "" {
			t.Fatalf("internal auth header leaked upstream: %q", got)
		}
		_, _ = w.Write([]byte(`{"execs":[]}`))
	}))
	t.Cleanup(upstream.Close)
	baseURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	publicKey, sign := testPoolTokenSigner(t)
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: projectID, PoolID: poolID},
		Runtime:               newProxyTestRuntime(t, baseURL),
		ControlPlanePublicKey: publicKey,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/project/project-1/pool/pool-1/sandboxes/sandbox-1/execs", nil)
	req.Header.Set("Authorization", "Bearer "+sign(projectID, poolID, sandboxID, ScopeExecRead))
	req.Header.Set(sandboxAgentAuthorizationHeader, "Bearer sandbox-token")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, body = %s", resp.Code, resp.Body.String())
	}
}

func TestSandboxHarnessHookProxyRequiresExecReadScope(t *testing.T) {
	projectID := "project-1"
	poolID := "pool-1"
	sandboxID := "sandbox-1"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/projects/project-1/sandboxes/sandbox-1/harness-hooks" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"hooks":[]}`))
	}))
	t.Cleanup(upstream.Close)
	baseURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	publicKey, sign := testPoolTokenSigner(t)
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: projectID, PoolID: poolID},
		Runtime:               newProxyTestRuntime(t, baseURL),
		ControlPlanePublicKey: publicKey,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/project/project-1/pool/pool-1/sandboxes/sandbox-1/harness-hooks", nil)
	req.Header.Set("Authorization", "Bearer "+sign(projectID, poolID, sandboxID, ScopeExecRead))
	req.Header.Set(sandboxAgentAuthorizationHeader, "Bearer sandbox-token")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, body = %s", resp.Code, resp.Body.String())
	}
}

// A terminal's screen, input, and wait are forwarded (ADR 0137): screen and
// wait on exec:read, a wait being a POST only for its body, input on exec:write.
func TestSandboxTerminalProxyScopes(t *testing.T) {
	projectID := "project-1"
	poolID := "pool-1"
	sandboxID := "sandbox-1"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(upstream.Close)
	baseURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, sign := testPoolTokenSigner(t)
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: projectID, PoolID: poolID},
		Runtime:               newProxyTestRuntime(t, baseURL),
		ControlPlanePublicKey: publicKey,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	for _, tc := range []struct {
		method, suffix, scope string
		want                  int
	}{
		{http.MethodGet, "screen", ScopeExecRead, http.StatusOK},
		{http.MethodPost, "wait", ScopeExecRead, http.StatusOK},
		{http.MethodPost, "input", ScopeExecRead, http.StatusForbidden},
		{http.MethodPost, "input", ScopeExecWrite, http.StatusOK},
	} {
		req := httptest.NewRequestWithContext(context.Background(), tc.method, "/api/project/project-1/pool/pool-1/sandboxes/sandbox-1/execs/exec-1/"+tc.suffix, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+sign(projectID, poolID, sandboxID, tc.scope))
		req.Header.Set(sandboxAgentAuthorizationHeader, "Bearer sandbox-token")
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != tc.want {
			t.Errorf("%s %s with %s = %d, want %d; body = %s", tc.method, tc.suffix, tc.scope, resp.Code, tc.want, resp.Body.String())
		}
	}
}

func TestSandboxExecProxyRequiresExecScope(t *testing.T) {
	projectID := "project-1"
	poolID := "pool-1"
	sandboxID := "sandbox-1"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/projects/project-1/sandboxes/sandbox-1/execs" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"execs":[]}`))
	}))
	t.Cleanup(upstream.Close)
	baseURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	publicKey, sign := testPoolTokenSigner(t)
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: projectID, PoolID: poolID},
		Runtime:               newProxyTestRuntime(t, baseURL),
		ControlPlanePublicKey: publicKey,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/project/project-1/pool/pool-1/sandboxes/sandbox-1/execs", nil)
	req.Header.Set("Authorization", "Bearer "+sign(projectID, poolID, sandboxID, ScopeExecRead))
	req.Header.Set(sandboxAgentAuthorizationHeader, "Bearer sandbox-token")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, body = %s", resp.Code, resp.Body.String())
	}
}

func TestSandboxExecProxyRejectsTerminalScope(t *testing.T) {
	projectID := "project-1"
	poolID := "pool-1"
	sandboxID := "sandbox-1"
	baseURL, err := url.Parse("http://sandbox.local")
	if err != nil {
		t.Fatal(err)
	}
	publicKey, sign := testPoolTokenSigner(t)
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: projectID, PoolID: poolID},
		Runtime:               newProxyTestRuntime(t, baseURL),
		ControlPlanePublicKey: publicKey,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/project/project-1/pool/pool-1/sandboxes/sandbox-1/execs", nil)
	req.Header.Set("Authorization", "Bearer "+sign(projectID, poolID, sandboxID, ScopeTerminalRead))
	req.Header.Set(sandboxAgentAuthorizationHeader, "Bearer sandbox-token")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("proxy status = %d, body = %s", resp.Code, resp.Body.String())
	}
}

// An exec against an archived sandbox must fail, and must fail with something
// the caller can act on. Before ADR 0022 §5 the auto-start latch swallowed every
// error and proxied anyway, which for an archived sandbox meant a 500 from the
// proxy about a missing IP address — true, and useless. The upstream here fails
// the test if it is reached at all: the request must not survive the latch.
func TestSandboxExecProxyRejectsArchivedSandbox(t *testing.T) {
	projectID := "project-1"
	poolID := "pool-1"
	sandboxID := "sandbox-1"
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Errorf("request reached the sandbox agent for an archived sandbox")
	}))
	t.Cleanup(upstream.Close)
	baseURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	runtime := sandboxruntime.NewMemorySandboxRuntime()
	if err := runtime.ArchiveSandbox(context.Background(), sandboxID); err != nil {
		t.Fatalf("archive: %v", err)
	}

	publicKey, sign := testPoolTokenSigner(t)
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: projectID, PoolID: poolID},
		Runtime:               proxyTestRuntime{MemorySandboxRuntime: runtime, baseURL: baseURL},
		ControlPlanePublicKey: publicKey,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/project/project-1/pool/pool-1/sandboxes/sandbox-1/execs", nil)
	req.Header.Set("Authorization", "Bearer "+sign(projectID, poolID, sandboxID, ScopeExecRead))
	req.Header.Set(sandboxAgentAuthorizationHeader, "Bearer sandbox-token")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusConflict {
		t.Fatalf("proxy status = %d, want 409; body = %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "unarchive") {
		t.Fatalf("response does not tell the caller what to do: %s", resp.Body.String())
	}
}

// proxyTestRuntime reaches every sandbox at baseURL's address, whichever port
// is asked for, and records the ports it was asked for in dialed when set.
type proxyTestRuntime struct {
	*sandboxruntime.MemorySandboxRuntime
	baseURL *url.URL
	dialed  chan<- int
}

// Model the sandbox behind the test upstream so auto-start sees it running.
func newProxyTestRuntime(t *testing.T, baseURL *url.URL) proxyTestRuntime {
	t.Helper()
	runtime := sandboxruntime.NewMemorySandboxRuntime()
	if _, err := runtime.CreateSandbox(t.Context(), &workerapimodel.PoolSandboxCreateRequest{SandboxId: "sandbox-1"}); err != nil {
		t.Fatal(err)
	}
	return proxyTestRuntime{MemorySandboxRuntime: runtime, baseURL: baseURL}
}

func (r proxyTestRuntime) SandboxDialer(_ context.Context, _ string, port int) (sandboxruntime.Dialer, error) {
	if r.dialed != nil {
		r.dialed <- port
	}
	return dialAddress(r.baseURL.Host), nil
}

func dialAddress(address string) sandboxruntime.Dialer {
	return func(ctx context.Context) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", address)
	}
}

func testPoolTokenSigner(t *testing.T) (string, func(projectID, poolID, sandboxID string, scopes ...string) string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	secretKey, err := paseto.NewV4AsymmetricSecretKeyFromEd25519(privateKey)
	if err != nil {
		t.Fatalf("load secret key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(publicKey), func(projectID, poolID, sandboxID string, scopes ...string) string {
		now := time.Now()
		token := paseto.NewToken()
		token.SetAudience(PoolAgentAudience)
		token.SetIssuedAt(now)
		token.SetNotBefore(now.Add(-time.Minute))
		token.SetExpiration(now.Add(time.Hour))
		token.SetString("project_id", projectID)
		token.SetString("pool_id", poolID)
		token.SetString("sandbox_id", sandboxID)
		if err := token.Set("scopes", scopes); err != nil {
			t.Fatalf("set scopes: %v", err)
		}
		return token.V4Sign(secretKey, nil)
	}
}

func TestSandboxTCPTunnelProxyRequiresTCPConnectScope(t *testing.T) {
	projectID := "project-1"
	poolID := "pool-1"
	sandboxID := "sandbox-1"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/projects/project-1/sandboxes/sandbox-1/tcp/attach" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))
	t.Cleanup(upstream.Close)
	baseURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	publicKey, sign := testPoolTokenSigner(t)
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: projectID, PoolID: poolID},
		Runtime:               newProxyTestRuntime(t, baseURL),
		ControlPlanePublicKey: publicKey,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	// exec:read/exec:write are not tcp:connect: the tunnel route must reject them.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/project/project-1/pool/pool-1/sandboxes/sandbox-1/tcp/attach?host=127.0.0.1&port=80", nil)
	req.Header.Set("Authorization", "Bearer "+sign(projectID, poolID, sandboxID, ScopeExecRead, ScopeExecWrite))
	req.Header.Set(sandboxAgentAuthorizationHeader, "Bearer sandbox-token")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("proxy status = %d, body = %s", resp.Code, resp.Body.String())
	}
}

func TestSandboxTCPTunnelProxyForwardsWithTCPConnectScope(t *testing.T) {
	projectID := "project-1"
	poolID := "pool-1"
	sandboxID := "sandbox-1"
	var gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/projects/project-1/sandboxes/sandbox-1/tcp/attach" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusBadGateway) // stand-in: no real listener behind this test's dial target
	}))
	t.Cleanup(upstream.Close)
	baseURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	publicKey, sign := testPoolTokenSigner(t)
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: projectID, PoolID: poolID},
		Runtime:               newProxyTestRuntime(t, baseURL),
		ControlPlanePublicKey: publicKey,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/project/project-1/pool/pool-1/sandboxes/sandbox-1/tcp/attach?host=127.0.0.1&port=80", nil)
	req.Header.Set("Authorization", "Bearer "+sign(projectID, poolID, sandboxID, ScopeTCPConnect))
	req.Header.Set(sandboxAgentAuthorizationHeader, "Bearer sandbox-token")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadGateway {
		t.Fatalf("proxy status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if gotQuery != "host=127.0.0.1&port=80" {
		t.Fatalf("upstream query = %q, want host/port forwarded", gotQuery)
	}
}

// The UDP tunnel is its own route with its own scope: tcp:connect does not
// reach it (ADR 0109 §4).
func TestSandboxUDPTunnelProxyRequiresUDPConnectScope(t *testing.T) {
	projectID := "project-1"
	poolID := "pool-1"
	sandboxID := "sandbox-1"
	var gotPath, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)
	baseURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	publicKey, sign := testPoolTokenSigner(t)
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: projectID, PoolID: poolID},
		Runtime:               newProxyTestRuntime(t, baseURL),
		ControlPlanePublicKey: publicKey,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	attach := func(scope string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/project/project-1/pool/pool-1/sandboxes/sandbox-1/udp/attach?host=127.0.0.1&port=53", nil)
		req.Header.Set("Authorization", "Bearer "+sign(projectID, poolID, sandboxID, scope))
		req.Header.Set(sandboxAgentAuthorizationHeader, "Bearer sandbox-token")
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		return resp
	}

	if resp := attach(ScopeTCPConnect); resp.Code != http.StatusForbidden {
		t.Fatalf("udp/attach with tcp:connect status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if gotPath != "" {
		t.Fatalf("a refused request reached the sandbox-agent at %q", gotPath)
	}

	if resp := attach(ScopeUDPConnect); resp.Code != http.StatusBadGateway {
		t.Fatalf("udp/attach with udp:connect status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if gotPath != "/api/projects/project-1/sandboxes/sandbox-1/udp/attach" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotQuery != "host=127.0.0.1&port=53" {
		t.Fatalf("upstream query = %q, want host/port forwarded", gotQuery)
	}
}

// The port proxy reaches the sandbox only through the dial its runtime
// supplies (ADR 0126 §5): the port asked for is the runtime's to resolve, the
// sandbox is told it is localhost on that port, and an upgrade carries on over
// the same connection.
func TestSandboxHTTPProxyReachesThePortThroughTheRuntimeDialer(t *testing.T) {
	projectID := "project-1"
	poolID := "pool-1"
	sandboxID := "sandbox-1"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws" {
			t.Errorf("upstream path = %q, want /ws", r.URL.Path)
		}
		if r.Host != "localhost:5173" {
			t.Errorf("upstream Host = %q, want localhost:5173", r.Host)
		}
		if r.Header.Get("Upgrade") != "echo" {
			t.Errorf("upstream Upgrade = %q, want echo", r.Header.Get("Upgrade"))
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		_ = rw.Flush()
		line, err := rw.ReadString('\n')
		if err != nil {
			t.Errorf("read upgraded stream: %v", err)
			return
		}
		_, _ = rw.WriteString(line)
		_ = rw.Flush()
	}))
	t.Cleanup(upstream.Close)
	baseURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	dialed := make(chan int, 1)
	runtime := newProxyTestRuntime(t, baseURL)
	runtime.dialed = dialed
	publicKey, sign := testPoolTokenSigner(t)
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: projectID, PoolID: poolID},
		Runtime:               runtime,
		ControlPlanePublicKey: publicKey,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	pool := httptest.NewServer(router)
	t.Cleanup(pool.Close)

	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp", pool.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial pool: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	request := "GET /api/project/project-1/pool/pool-1/sandboxes/sandbox-1/http/5173/ws HTTP/1.1\r\n" +
		"Host: pool\r\n" +
		"Authorization: Bearer " + sign(projectID, poolID, sandboxID, ScopeSandboxHTTP) + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: echo\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("proxy status = %d, want 101; body = %s", resp.StatusCode, body)
	}
	if port := <-dialed; port != 5173 {
		t.Fatalf("runtime asked to dial port %d, want 5173", port)
	}
	if _, err := conn.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write upgraded stream: %v", err)
	}
	echoed, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read upgraded stream: %v", err)
	}
	if echoed != "hello\n" {
		t.Fatalf("echoed = %q, want hello", echoed)
	}
}

// A sandbox the proxy cannot reach is a failure worth a line and a 502.
func TestSandboxProxyReportsAnUnreachableSandbox(t *testing.T) {
	logged := captureProxyLog(t)
	upstream := httptest.NewServer(http.NotFoundHandler())
	address := upstream.Listener.Addr().String()
	upstream.Close()

	recorder := httptest.NewRecorder()
	sandboxProxy(dialAddress(address), sandboxruntime.HTTPURL(sandboxruntime.SandboxAgentPort, "/"), "").ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/sandbox", nil))

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	if !strings.Contains(logged.String(), "http: proxy error") {
		t.Fatalf("log = %q, want the proxy error reported", logged.String())
	}
}

// The control plane cancels its request whenever its own client leaves. That
// is not a failure, so nothing is logged for it.
func TestSandboxProxyIgnoresAClientThatWentAway(t *testing.T) {
	logged := captureProxyLog(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the proxy reached the sandbox for a request whose client had gone")
	}))
	t.Cleanup(upstream.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/sandbox", nil)
	sandboxProxy(dialAddress(upstream.Listener.Addr().String()), sandboxruntime.HTTPURL(sandboxruntime.SandboxAgentPort, "/"), "").ServeHTTP(httptest.NewRecorder(), request)

	if logged.Len() != 0 {
		t.Fatalf("log = %q, want nothing for a client that went away", logged.String())
	}
}

// captureProxyLog redirects the standard logger, which is where the proxy
// reports a failure, for the length of a test.
func captureProxyLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &buf
}
