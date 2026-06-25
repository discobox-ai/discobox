package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"

	"github.com/obot-platform/discobox/worker-agent/sandboxruntime"
)

func TestSandboxAgentProxyRewritesToSandboxAgentAndForwardsDownstreamToken(t *testing.T) {
	projectID := "project-1"
	workerID := "worker-1"
	sandboxID := "sandbox-1"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/projects/project-1/sandboxes/sandbox-1/agent-terminals" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sandbox-token" {
			t.Fatalf("upstream authorization = %q", got)
		}
		if got := r.Header.Get(sandboxAgentAuthorizationHeader); got != "" {
			t.Fatalf("internal auth header leaked upstream: %q", got)
		}
		_, _ = w.Write([]byte(`{"terminals":[]}`))
	}))
	t.Cleanup(upstream.Close)
	baseURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	publicKey, sign := testWorkerTokenSigner(t)
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: projectID, WorkerID: workerID},
		Runtime:               proxyTestRuntime{MemorySandboxRuntime: sandboxruntime.NewMemorySandboxRuntime(), baseURL: baseURL},
		ControlPlanePublicKey: publicKey,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/project/project-1/worker/worker-1/sandboxes/sandbox-1/agent-terminals", nil)
	req.Header.Set("Authorization", "Bearer "+sign(projectID, workerID, sandboxID, ScopeTerminalRead))
	req.Header.Set(sandboxAgentAuthorizationHeader, "Bearer sandbox-token")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, body = %s", resp.Code, resp.Body.String())
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

func testWorkerTokenSigner(t *testing.T) (string, func(projectID, workerID, sandboxID string, scopes ...string) string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	secretKey, err := paseto.NewV4AsymmetricSecretKeyFromEd25519(privateKey)
	if err != nil {
		t.Fatalf("load secret key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(publicKey), func(projectID, workerID, sandboxID string, scopes ...string) string {
		now := time.Now()
		token := paseto.NewToken()
		token.SetAudience(WorkerAgentAudience)
		token.SetIssuedAt(now)
		token.SetNotBefore(now.Add(-time.Minute))
		token.SetExpiration(now.Add(time.Hour))
		token.SetString("project_id", projectID)
		token.SetString("worker_id", workerID)
		token.SetString("sandbox_id", sandboxID)
		if err := token.Set("scopes", scopes); err != nil {
			t.Fatalf("set scopes: %v", err)
		}
		return token.V4Sign(secretKey, nil)
	}
}
