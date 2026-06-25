package workeragent_test

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
	workeragent "github.com/obot-platform/discobox/worker-agent"
	workerclient "github.com/obot-platform/discobox/worker-agent/api/gen"
	workerapimodel "github.com/obot-platform/discobox/worker-agent/api/model"
	"github.com/obot-platform/discobox/worker-agent/sandboxruntime"
	workeragentserver "github.com/obot-platform/discobox/worker-agent/server"
	"github.com/obot-platform/discobox/worker-agent/workerauth"
)

func TestRunRegistersWorkerWithGeneratedPublicKey(t *testing.T) {
	ctx := context.Background()
	client := &recordingClient{resp: &workeragent.RegisterResponse{}}
	registration, err := workeragent.Run(ctx, workeragent.Config{
		Bootstrap: workeragent.Bootstrap{
			ControlPlaneURL: "https://control.example",
			ProjectID:       "project-1",
			SandboxID:       "sandbox-1",
			WorkerID:        "worker-1",
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
	t.Setenv(workeragent.EnvControlPlaneURL, "")

	bootstrap := workeragent.FromEnv()

	if bootstrap.ControlPlaneURL != controlplane.DefaultURL("localhost", controlplane.DefaultPort) {
		t.Fatalf("control plane URL = %q, want default localhost URL", bootstrap.ControlPlaneURL)
	}
}

func TestFromEnvReadsHostMountPrefix(t *testing.T) {
	t.Setenv(workeragent.EnvHostMountPrefix, "/host")

	bootstrap := workeragent.FromEnv()

	if bootstrap.HostMountPrefix != "/host" {
		t.Fatalf("host mount prefix = %q, want /host", bootstrap.HostMountPrefix)
	}
}

func TestHTTPClientRegistersWorker(t *testing.T) {
	var got workeragent.RegisterRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workers/register" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if err := json.NewEncoder(w).Encode(workeragent.RegisterResponse{}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	resp, err := workeragent.NewHTTPClient(server.URL, workeragent.WithHTTPClient(server.Client())).RegisterWorker(context.Background(), workeragent.RegisterRequest{
		ProjectID:      "project-1",
		SandboxID:      "sandbox-1",
		BootstrapToken: "bootstrap-token",
		PublicKey:      "public-key",
		KeyType:        "ed25519",
	})
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}
	if resp == nil {
		t.Fatal("register response is nil")
	}
	if got.ProjectID != "project-1" {
		t.Fatalf("project ID = %q", got.ProjectID)
	}
	if got.SandboxID != "sandbox-1" {
		t.Fatalf("sandbox ID = %q", got.SandboxID)
	}
	if got.WorkerID != "" {
		t.Fatalf("worker ID = %q, want empty", got.WorkerID)
	}
}

func TestHTTPClientUpdatesWorkerStatusByPath(t *testing.T) {
	var got map[string]any
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	publicKeyText, err := workerauth.EncodePublicKey(publicKey)
	if err != nil {
		t.Fatalf("encode public key: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workers/worker-1/status" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || scheme != "Bearer" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		claims, err := workerauth.VerifyToken(publicKeyText, token)
		if err != nil {
			t.Fatalf("verify token: %v", err)
		}
		if claims.ProjectID != "project-1" || claims.WorkerID != "worker-1" {
			t.Fatalf("claims = %#v", claims)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
	}))
	defer server.Close()

	err = workeragent.NewHTTPClient(server.URL, workeragent.WithHTTPClient(server.Client())).UpdateWorkerStatus(context.Background(), workeragent.StatusRequest{
		ProjectID:             "project-1",
		WorkerID:              "worker-1",
		PrivateKey:            privateKey,
		Ready:                 true,
		Schedulable:           true,
		AvailableCPUVCPUs:     1,
		AvailableMemoryBytes:  2,
		AvailableStorageBytes: 3,
	})
	if err != nil {
		t.Fatalf("update worker status: %v", err)
	}
	if _, ok := got["workerId"]; ok {
		t.Fatalf("status body includes workerId: %#v", got)
	}
}

func TestWorkerSandboxHandlersValidateIdentityAndOperateOnRuntime(t *testing.T) {
	runtime := workeragent.NewMemorySandboxRuntime()
	controlPlaneKey, signToken := workerAgentTestSigner(t)
	token := signToken("project-1", "worker-1", "", workeragentserver.ScopeSandboxRead, workeragentserver.ScopeSandboxWrite)
	readOnlyToken := signToken("project-1", "worker-1", "", workeragentserver.ScopeSandboxRead)
	server := httptest.NewServer(workeragent.NewSandboxHandler(workeragent.Bootstrap{ProjectID: "project-1", WorkerID: "worker-1", ControlPlaneKey: controlPlaneKey}, runtime))
	defer server.Close()
	unauthenticated := server.Client()
	unauthReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/api/project/project-1/worker/worker-1/sandboxes", nil)
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
	readOnlyReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/api/project/project-1/worker/worker-1/sandboxes", nil)
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

	client, err := workerclient.NewClient(server.URL, testWorkerSecuritySource{token: token}, workerclient.WithClient(server.Client()))
	if err != nil {
		t.Fatalf("new worker client: %v", err)
	}

	created, err := client.WorkerCreateSandbox(context.Background(), &workerapimodel.WorkerSandboxCreateRequest{SandboxId: "sandbox-1", Image: workerclient.NewOptString("alpine")}, workerclient.WorkerCreateSandboxParams{ProjectId: "project-1", WorkerId: "worker-1"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if created.SandboxID != "sandbox-1" || sandboxruntime.Status(created.Status) != sandboxruntime.StatusRunning {
		t.Fatalf("created sandbox = %#v", created)
	}

	started, err := client.WorkerStartSandbox(context.Background(), &workerapimodel.WorkerSandboxOperationRequest{}, workerclient.WorkerStartSandboxParams{ProjectId: "project-1", WorkerId: "worker-1", SandboxId: "sandbox-1"})
	if err != nil {
		t.Fatalf("start already-running sandbox: %v", err)
	}
	if sandboxruntime.Status(started.Status) != sandboxruntime.StatusRunning {
		t.Fatalf("started status = %q", started.Status)
	}

	stopped, err := client.WorkerStopSandbox(context.Background(), &workerapimodel.WorkerSandboxOperationRequest{}, workerclient.WorkerStopSandboxParams{ProjectId: "project-1", WorkerId: "worker-1", SandboxId: "sandbox-1"})
	if err != nil {
		t.Fatalf("stop sandbox: %v", err)
	}
	if sandboxruntime.Status(stopped.Status) != sandboxruntime.StatusStopped {
		t.Fatalf("stopped status = %q", stopped.Status)
	}

	_, err = client.WorkerGetSandbox(context.Background(), workerclient.WorkerGetSandboxParams{ProjectId: "project-other", WorkerId: "worker-1", SandboxId: "sandbox-1"})
	if err == nil {
		t.Fatalf("expected wrong project request to fail")
	}
}

func TestWorkerSandboxGitRepositoryRouteServesCheckedOutRepository(t *testing.T) {
	ctx := context.Background()
	runtime := workeragent.NewMemorySandboxRuntime()
	if _, err := runtime.CreateSandbox(ctx, &workerapimodel.WorkerSandboxCreateRequest{SandboxId: "sandbox-1", Image: workerclient.NewOptString("alpine")}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	repo := filepath.Join(t.TempDir(), "primary")
	initGitRepo(t, repo, "one\n")
	runtime.SetGitRepositoryPath("sandbox-1", "primary", repo)

	controlPlaneKey, signToken := workerAgentTestSigner(t)
	readToken := signToken("project-1", "worker-1", "sandbox-1", workeragentserver.ScopeSandboxRead)
	writeToken := signToken("project-1", "worker-1", "sandbox-1", workeragentserver.ScopeSandboxRead, workeragentserver.ScopeSandboxWrite)
	server := httptest.NewServer(workeragent.NewSandboxHandler(workeragent.Bootstrap{ProjectID: "project-1", WorkerID: "worker-1", ControlPlaneKey: controlPlaneKey}, runtime))
	defer server.Close()

	gitURL := server.URL + "/api/project/project-1/worker/worker-1/sandboxes/sandbox-1/git-repositories/primary.git"
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
	req  workeragent.RegisterRequest
	resp *workeragent.RegisterResponse
}

func (c *recordingClient) RegisterWorker(_ context.Context, req workeragent.RegisterRequest) (*workeragent.RegisterResponse, error) {
	c.req = req
	return c.resp, nil
}

type testWorkerSecuritySource struct {
	token string
}

func (s testWorkerSecuritySource) WorkerBearerAuth(context.Context, workerclient.OperationName) (workerclient.WorkerBearerAuth, error) {
	return workerclient.WorkerBearerAuth{Token: s.token}, nil
}

func workerAgentTestSigner(t *testing.T) (string, func(projectID, workerID, sandboxID string, scopes ...string) string) {
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
		token.SetAudience(workeragentserver.WorkerAgentAudience)
		token.SetIssuedAt(now)
		token.SetNotBefore(now.Add(-time.Minute))
		token.SetExpiration(now.Add(time.Hour))
		token.SetString("project_id", projectID)
		token.SetString("worker_id", workerID)
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
