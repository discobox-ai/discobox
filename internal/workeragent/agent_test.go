package workeragent_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/obot-platform/discobox/internal/sandbox"
	"github.com/obot-platform/discobox/internal/workeragent"
	workerclient "github.com/obot-platform/discobox/internal/workeragent/client/gen"
)

func TestRunRegistersWorkerWithGeneratedPublicKey(t *testing.T) {
	ctx := context.Background()
	client := &recordingClient{resp: &workeragent.RegisterResponse{AuthToken: "auth-token"}}
	registration, err := workeragent.Run(ctx, workeragent.Config{
		Bootstrap: workeragent.Bootstrap{
			ControlPlaneURL: "https://control.example",
			TenantID:        "tenant-1",
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
	if registration.AuthToken != "auth-token" {
		t.Fatalf("auth token = %q", registration.AuthToken)
	}
	publicKey, err := base64.StdEncoding.DecodeString(client.req.PublicKey)
	if err != nil {
		t.Fatalf("decode public key: %v", err)
	}
	if len(publicKey) != ed25519.PublicKeySize {
		t.Fatalf("public key length = %d", len(publicKey))
	}
	if client.req.TenantID != "tenant-1" {
		t.Fatalf("tenant ID = %q", client.req.TenantID)
	}
}

func TestHTTPClientRegistersWorker(t *testing.T) {
	var got workeragent.RegisterRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workers/register" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.Header.Get("X-Discobox-Tenant-ID") != "tenant-1" {
			t.Fatalf("tenant header = %q", r.Header.Get("X-Discobox-Tenant-ID"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(workeragent.RegisterResponse{AuthToken: "auth-token"})
	}))
	defer server.Close()

	resp, err := workeragent.NewHTTPClient(server.URL, workeragent.WithHTTPClient(server.Client())).RegisterWorker(context.Background(), workeragent.RegisterRequest{
		TenantID:       "tenant-1",
		WorkerID:       "worker-1",
		BootstrapToken: "bootstrap-token",
		PublicKey:      "public-key",
		KeyType:        "ed25519",
	})
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}
	if resp.AuthToken != "auth-token" {
		t.Fatalf("auth token = %q", resp.AuthToken)
	}
	if got.WorkerID != "worker-1" {
		t.Fatalf("worker ID = %q", got.WorkerID)
	}
}

func TestWorkerSandboxHandlersValidateIdentityAndOperateOnRuntime(t *testing.T) {
	runtime := workeragent.NewMemorySandboxRuntime()
	server := httptest.NewServer(workeragent.NewSandboxHandler(workeragent.Bootstrap{ProjectID: "project-1", WorkerID: "worker-1"}, runtime, "worker-api-token"))
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

	client, err := workerclient.NewClient(server.URL, testWorkerSecuritySource{token: "worker-api-token"}, workerclient.WithClient(server.Client()))
	if err != nil {
		t.Fatalf("new worker client: %v", err)
	}

	created, err := client.WorkerCreateSandbox(context.Background(), &workerclient.WorkerSandboxCreateRequest{SandboxId: "sandbox-1", Image: workerclient.NewOptString("alpine")}, workerclient.WorkerCreateSandboxParams{ProjectId: "project-1", WorkerId: "worker-1"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if created.SandboxID != "sandbox-1" || sandbox.Status(created.Status) != sandbox.StatusRunning {
		t.Fatalf("created sandbox = %#v", created)
	}

	started, err := client.WorkerStartSandbox(context.Background(), &workerclient.WorkerSandboxOperationRequest{}, workerclient.WorkerStartSandboxParams{ProjectId: "project-1", WorkerId: "worker-1", SandboxId: "sandbox-1"})
	if err != nil {
		t.Fatalf("start already-running sandbox: %v", err)
	}
	if sandbox.Status(started.Status) != sandbox.StatusRunning {
		t.Fatalf("started status = %q", started.Status)
	}

	stopped, err := client.WorkerStopSandbox(context.Background(), &workerclient.WorkerSandboxOperationRequest{}, workerclient.WorkerStopSandboxParams{ProjectId: "project-1", WorkerId: "worker-1", SandboxId: "sandbox-1"})
	if err != nil {
		t.Fatalf("stop sandbox: %v", err)
	}
	if sandbox.Status(stopped.Status) != sandbox.StatusStopped {
		t.Fatalf("stopped status = %q", stopped.Status)
	}

	_, err = client.WorkerGetSandbox(context.Background(), workerclient.WorkerGetSandboxParams{ProjectId: "project-other", WorkerId: "worker-1", SandboxId: "sandbox-1"})
	if err == nil {
		t.Fatalf("expected wrong project request to fail")
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
