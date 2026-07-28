package poolagent_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"

	"github.com/obot-platform/discobox/controlplane"
	poolagent "github.com/obot-platform/discobox/pool-agent"
	workerclient "github.com/obot-platform/discobox/pool-agent/api/gen"
	workerapimodel "github.com/obot-platform/discobox/pool-agent/api/model"
	"github.com/obot-platform/discobox/pool-agent/poolauth"
	"github.com/obot-platform/discobox/pool-agent/sandboxruntime"
	poolagentserver "github.com/obot-platform/discobox/pool-agent/server"
)

func TestRunRegistersPoolWithGeneratedPublicKey(t *testing.T) {
	ctx := context.Background()
	client := &recordingClient{resp: &poolagent.RegisterResponse{}}
	registration, err := poolagent.Run(ctx, poolagent.Config{
		Bootstrap: poolagent.Bootstrap{
			ControlPlaneURL: "https://control.example",
			ProjectID:       "project-1",
			PoolID:          "pool-1",
			Token:           "bootstrap-token",
		},
		Client: client,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(registration.PrivateKey) != ed25519.PrivateKeySize {
		t.Fatalf("private key length = %d", len(registration.PrivateKey))
	}
	publicKey, err := base64.StdEncoding.DecodeString(client.req.PublicKey)
	if err != nil {
		t.Fatalf("decode public key: %v", err)
	}
	if len(publicKey) != ed25519.PublicKeySize {
		t.Fatalf("public key length = %d", len(publicKey))
	}
}

func TestFromEnvDefaultsControlPlaneURL(t *testing.T) {
	t.Setenv(poolagent.EnvControlPlaneURL, "")

	bootstrap := poolagent.FromEnv()

	if bootstrap.ControlPlaneURL != controlplane.DefaultURL("localhost", controlplane.DefaultPort) {
		t.Fatalf("control plane URL = %q, want default localhost URL", bootstrap.ControlPlaneURL)
	}
}

func TestFromEnvReadsHostMountPrefix(t *testing.T) {
	t.Setenv(poolagent.EnvHostMountPrefix, "/host")

	bootstrap := poolagent.FromEnv()

	if bootstrap.HostMountPrefix != "/host" {
		t.Fatalf("host mount prefix = %q, want /host", bootstrap.HostMountPrefix)
	}
}

func TestFromEnvReadsTransportURLs(t *testing.T) {
	t.Setenv(poolagent.EnvControlPlaneURL, "vsock://2:3001")
	t.Setenv(poolagent.EnvAgentListenURL, "vsock://:3002")

	bootstrap := poolagent.FromEnv()

	if bootstrap.ControlPlaneURL != "vsock://2:3001" || bootstrap.AgentListenURL != "vsock://:3002" {
		t.Fatalf("transport URLs = control:%q agent:%q, want vsock://2:3001 and vsock://:3002",
			bootstrap.ControlPlaneURL, bootstrap.AgentListenURL)
	}
}

// The transport now lives in the URL, so an unusable transport must be rejected
// by the same validation that checks the rest of the bootstrap contract.
func TestBootstrapRejectsUnusableTransportURLs(t *testing.T) {
	base := poolagent.Bootstrap{
		ControlPlaneURL: "http://control.example",
		ProjectID:       "project-1",
		PoolID:          "pool-1",
		Token:           "token-1",
	}
	for name, mutate := range map[string]func(*poolagent.Bootstrap){
		"unknown control plane scheme": func(b *poolagent.Bootstrap) { b.ControlPlaneURL = "ftp://nope" },
		"privileged vsock port":        func(b *poolagent.Bootstrap) { b.ControlPlaneURL = "vsock://2:1" },
		"unknown agent listen scheme":  func(b *poolagent.Bootstrap) { b.AgentListenURL = "ftp://nope" },
		"agent listen without port":    func(b *poolagent.Bootstrap) { b.AgentListenURL = "vsock://2" },
	} {
		t.Run(name, func(t *testing.T) {
			bootstrap := base
			mutate(&bootstrap)
			if err := bootstrap.Validate(); err == nil {
				t.Fatal("Validate accepted an unusable transport URL")
			}
		})
	}
}

// A VSOCK control plane URL must still satisfy validation, since that is what
// libkrun renders.
func TestBootstrapAcceptsVSOCKTransportURLs(t *testing.T) {
	bootstrap := poolagent.Bootstrap{
		ControlPlaneURL: "vsock://2:3001",
		AgentListenURL:  "vsock://:3002",
		ProjectID:       "project-1",
		PoolID:          "pool-1",
		Token:           "token-1",
	}
	if err := bootstrap.Validate(); err != nil {
		t.Fatalf("Validate rejected VSOCK transport URLs: %v", err)
	}
}

func TestHTTPClientRegistersPool(t *testing.T) {
	var got poolagent.RegisterRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pools/register" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if err := json.NewEncoder(w).Encode(poolagent.RegisterResponse{}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	resp, err := poolagent.NewHTTPClient(server.URL, poolagent.WithHTTPClient(server.Client())).RegisterPool(context.Background(), poolagent.RegisterRequest{
		ProjectID:      "project-1",
		PoolID:         "pool-1",
		BootstrapToken: "bootstrap-token",
		PublicKey:      "public-key",
		KeyType:        "ed25519",
	})
	if err != nil {
		t.Fatalf("register pool: %v", err)
	}
	if resp == nil {
		t.Fatal("register response is nil")
	}
	if got.ProjectID != "project-1" {
		t.Fatalf("project ID = %q", got.ProjectID)
	}
	if got.PoolID != "pool-1" {
		t.Fatalf("pool ID = %q", got.PoolID)
	}
}

func TestHTTPClientUpdatesPoolStatusByPath(t *testing.T) {
	var got map[string]any
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	publicKeyText, err := poolauth.EncodePublicKey(publicKey)
	if err != nil {
		t.Fatalf("encode public key: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pools/pool-1/status" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || scheme != "Bearer" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		claims, err := poolauth.VerifyToken(publicKeyText, token)
		if err != nil {
			t.Fatalf("verify token: %v", err)
		}
		if claims.ProjectID != "project-1" || claims.PoolID != "pool-1" {
			t.Fatalf("claims = %#v", claims)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
	}))
	defer server.Close()

	err = poolagent.NewHTTPClient(server.URL, poolagent.WithHTTPClient(server.Client())).UpdatePoolStatus(context.Background(), poolagent.StatusRequest{
		ProjectID:             "project-1",
		PoolID:                "pool-1",
		PrivateKey:            privateKey,
		Ready:                 true,
		Schedulable:           true,
		AvailableCPUVCPUs:     1,
		AvailableMemoryBytes:  2,
		AvailableStorageBytes: 3,
	})
	if err != nil {
		t.Fatalf("update pool status: %v", err)
	}
	if _, ok := got["poolId"]; ok {
		t.Fatalf("status body includes poolId: %#v", got)
	}
}

func TestHTTPClientReportsSandboxRemoved(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	publicKeyText, err := poolauth.EncodePublicKey(publicKey)
	if err != nil {
		t.Fatalf("encode public key: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pools/pool-1/sandbox-removed" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		claims, err := poolauth.VerifyToken(publicKeyText, token)
		if err != nil {
			t.Fatalf("verify token: %v", err)
		}
		if claims.ProjectID != "project-1" || claims.PoolID != "pool-1" {
			t.Fatalf("claims = %#v", claims)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["sandboxId"] != "sandbox-1" {
			t.Fatalf("body = %#v", body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	err = poolagent.NewHTTPClient(server.URL, poolagent.WithHTTPClient(server.Client())).ReportSandboxRemoved(context.Background(), poolagent.SandboxRemovalRequest{
		ProjectID: "project-1", PoolID: "pool-1", PrivateKey: privateKey, SandboxID: "sandbox-1",
	})
	if err != nil {
		t.Fatalf("report sandbox removed: %v", err)
	}
}

func TestPoolSandboxHandlersValidateIdentityAndOperateOnRuntime(t *testing.T) {
	runtime := poolagent.NewMemorySandboxRuntime()
	controlPlaneKey, signToken := workerAgentTestSigner(t)
	token := signToken("project-1", "pool-1", "", poolagentserver.ScopeSandboxRead, poolagentserver.ScopeSandboxWrite)
	readOnlyToken := signToken("project-1", "pool-1", "", poolagentserver.ScopeSandboxRead)
	server := httptest.NewServer(poolagent.NewSandboxHandler(poolagent.Bootstrap{ProjectID: "project-1", PoolID: "pool-1", ControlPlaneKey: controlPlaneKey}, runtime))
	defer server.Close()
	unauthenticated := server.Client()
	unauthReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/api/project/project-1/pool/pool-1/sandboxes", nil)
	if err != nil {
		t.Fatalf("new unauthenticated request: %v", err)
	}
	unauthResp, err := unauthenticated.Do(unauthReq)
	if err != nil {
		t.Fatalf("unauthenticated request: %v", err)
	}
	_ = unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", unauthResp.StatusCode, http.StatusUnauthorized)
	}
	readOnlyReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/api/project/project-1/pool/pool-1/sandboxes", nil)
	if err != nil {
		t.Fatalf("new read-only request: %v", err)
	}
	readOnlyReq.Header.Set("Authorization", "Bearer "+readOnlyToken)
	readOnlyResp, err := server.Client().Do(readOnlyReq)
	if err != nil {
		t.Fatalf("read-only write request: %v", err)
	}
	_ = readOnlyResp.Body.Close()
	if readOnlyResp.StatusCode != http.StatusForbidden {
		t.Fatalf("read-only write status = %d, want %d", readOnlyResp.StatusCode, http.StatusForbidden)
	}

	client, err := workerclient.NewClient(server.URL, testPoolSecuritySource{token: token}, workerclient.WithClient(server.Client()))
	if err != nil {
		t.Fatalf("new pool client: %v", err)
	}

	created, err := client.PoolCreateSandbox(context.Background(), &workerapimodel.PoolSandboxCreateRequest{
		SandboxId: "sandbox-1",
		Config: workerapimodel.SandboxConfig{
			Image: workerclient.NewOptString("alpine"),
		},
	}, workerclient.PoolCreateSandboxParams{ProjectId: "project-1", PoolId: "pool-1"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if created.SandboxId != "sandbox-1" || sandboxruntime.Status(created.Runtime.Status) != sandboxruntime.StatusRunning {
		t.Fatalf("created sandbox = %#v", created)
	}

	started, err := client.PoolStartSandbox(context.Background(), &workerapimodel.PoolSandboxOperationRequest{}, workerclient.PoolStartSandboxParams{ProjectId: "project-1", PoolId: "pool-1", SandboxId: "sandbox-1"})
	if err != nil {
		t.Fatalf("start already-running sandbox: %v", err)
	}
	if sandboxruntime.Status(started.Runtime.Status) != sandboxruntime.StatusRunning {
		t.Fatalf("started status = %q", started.Runtime.Status)
	}

	stopped, err := client.PoolStopSandbox(context.Background(), &workerapimodel.PoolSandboxOperationRequest{}, workerclient.PoolStopSandboxParams{ProjectId: "project-1", PoolId: "pool-1", SandboxId: "sandbox-1"})
	if err != nil {
		t.Fatalf("stop sandbox: %v", err)
	}
	if sandboxruntime.Status(stopped.Runtime.Status) != sandboxruntime.StatusStopped {
		t.Fatalf("stopped status = %q", stopped.Runtime.Status)
	}

	_, err = client.PoolGetSandbox(context.Background(), workerclient.PoolGetSandboxParams{ProjectId: "project-other", PoolId: "pool-1", SandboxId: "sandbox-1"})
	if err == nil {
		t.Fatalf("expected wrong project request to fail")
	}
}

func TestPoolSandboxGitRepositoryRouteServesCheckedOutRepository(t *testing.T) {
	ctx := context.Background()
	runtime := poolagent.NewMemorySandboxRuntime()
	if _, err := runtime.CreateSandbox(ctx, &workerapimodel.PoolSandboxCreateRequest{
		SandboxId: "sandbox-1",
		Config: workerapimodel.SandboxConfig{
			Image: workerclient.NewOptString("alpine"),
		},
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	repo := filepath.Join(t.TempDir(), "primary")
	initGitRepo(t, repo, "one\n")
	runtime.SetGitRepositoryPath("sandbox-1", "primary", repo)

	controlPlaneKey, signToken := workerAgentTestSigner(t)
	readToken := signToken("project-1", "pool-1", "sandbox-1", poolagentserver.ScopeSandboxRead)
	writeToken := signToken("project-1", "pool-1", "sandbox-1", poolagentserver.ScopeSandboxRead, poolagentserver.ScopeSandboxWrite)
	server := httptest.NewServer(poolagent.NewSandboxHandler(poolagent.Bootstrap{ProjectID: "project-1", PoolID: "pool-1", ControlPlaneKey: controlPlaneKey}, runtime))
	defer server.Close()

	gitURL := server.URL + "/api/project/project-1/pool/pool-1/sandboxes/sandbox-1/git-repositories/primary.git"
	if out := gitOutput(t, "", "-c", "http.extraHeader=Authorization: Bearer "+readToken, "ls-remote", gitURL, "HEAD"); !strings.Contains(out, "HEAD") {
		t.Fatalf("ls-remote output = %q, want HEAD", out)
	}

	clientRepo := filepath.Join(t.TempDir(), "client")
	git(t, "", "-c", "http.extraHeader=Authorization: Bearer "+writeToken, "clone", gitURL, clientRepo)
	if err := os.WriteFile(filepath.Join(clientRepo, "README.md"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, clientRepo, "add", "README.md")
	git(t, clientRepo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "two")
	git(t, clientRepo, "-c", "http.extraHeader=Authorization: Bearer "+writeToken, "push", "origin", "main")

	data, err := os.ReadFile(filepath.Join(repo, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "two\n" {
		t.Fatalf("sandbox worktree README = %q, want pushed content", string(data))
	}

	readOnlyClientRepo := filepath.Join(t.TempDir(), "readonly-client")
	git(t, "", "-c", "http.extraHeader=Authorization: Bearer "+readToken, "clone", gitURL, readOnlyClientRepo)
	if err := os.WriteFile(filepath.Join(readOnlyClientRepo, "README.md"), []byte("three\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, readOnlyClientRepo, "add", "README.md")
	git(t, readOnlyClientRepo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "three")
	if err := gitErr(readOnlyClientRepo, "-c", "http.extraHeader=Authorization: Bearer "+readToken, "push", "origin", "main"); err == nil {
		t.Fatal("read-only token push succeeded, want failure")
	}
}

type recordingClient struct {
	req  poolagent.RegisterRequest
	resp *poolagent.RegisterResponse
}

func (c *recordingClient) RegisterPool(_ context.Context, req poolagent.RegisterRequest) (*poolagent.RegisterResponse, error) {
	c.req = req
	return c.resp, nil
}

type testPoolSecuritySource struct {
	token string
}

func (s testPoolSecuritySource) PoolBearerAuth(context.Context, workerclient.OperationName) (workerclient.PoolBearerAuth, error) {
	return workerclient.PoolBearerAuth{Token: s.token}, nil
}

func workerAgentTestSigner(t *testing.T) (string, func(projectID, poolID, sandboxID string, scopes ...string) string) {
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
		token.SetAudience(poolagentserver.PoolAgentAudience)
		token.SetIssuedAt(now)
		token.SetNotBefore(now.Add(-time.Minute))
		token.SetExpiration(now.Add(time.Hour))
		token.SetString("project_id", projectID)
		token.SetString("pool_id", poolID)
		if sandboxID != "" {
			token.SetString("sandbox_id", sandboxID)
		}
		if err := token.Set("scopes", scopes); err != nil {
			t.Fatalf("set scopes: %v", err)
		}
		return token.V4Sign(secretKey, nil)
	}
}

func initGitRepo(t *testing.T, dir, readme string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "README.md")
	git(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	if err := gitErr(dir, args...); err != nil {
		t.Fatal(err)
	}
}

func gitErr(dir string, args ...string) error {
	cmd := exec.CommandContext(context.Background(), "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
}
