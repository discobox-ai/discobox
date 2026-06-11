package workeragent_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/obot-platform/disco2/internal/workeragent"
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
		if r.Header.Get("X-Disco2-Tenant-ID") != "tenant-1" {
			t.Fatalf("tenant header = %q", r.Header.Get("X-Disco2-Tenant-ID"))
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

type recordingClient struct {
	req  workeragent.RegisterRequest
	resp *workeragent.RegisterResponse
}

func (c *recordingClient) RegisterWorker(_ context.Context, req workeragent.RegisterRequest) (*workeragent.RegisterResponse, error) {
	c.req = req
	return c.resp, nil
}
