package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"

	"github.com/obot-platform/discobox/pool-agent/sandboxruntime"
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
		Runtime:               proxyTestRuntime{MemorySandboxRuntime: sandboxruntime.NewMemorySandboxRuntime(), baseURL: baseURL},
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
		Runtime:               proxyTestRuntime{MemorySandboxRuntime: sandboxruntime.NewMemorySandboxRuntime(), baseURL: baseURL},
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
		Runtime:               proxyTestRuntime{MemorySandboxRuntime: sandboxruntime.NewMemorySandboxRuntime(), baseURL: baseURL},
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
		Runtime:               proxyTestRuntime{MemorySandboxRuntime: sandboxruntime.NewMemorySandboxRuntime(), baseURL: baseURL},
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

type proxyTestRuntime struct {
	*sandboxruntime.MemorySandboxRuntime
	baseURL *url.URL
}

func (r proxyTestRuntime) HTTPBaseURL(context.Context, string, int) (*url.URL, error) {
	copied := *r.baseURL
	return &copied, nil
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
		Runtime:               proxyTestRuntime{MemorySandboxRuntime: sandboxruntime.NewMemorySandboxRuntime(), baseURL: baseURL},
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
		Runtime:               proxyTestRuntime{MemorySandboxRuntime: sandboxruntime.NewMemorySandboxRuntime(), baseURL: baseURL},
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
