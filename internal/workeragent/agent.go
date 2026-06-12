// Package workeragent implements the in-guest worker startup registration flow.
package workeragent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	EnvControlPlaneURL = "DISCOBOX_CONTROL_PLANE_URL"
	EnvTenantID        = "DISCOBOX_TENANT_ID"
	EnvProjectID       = "DISCOBOX_PROJECT_ID"
	EnvSandboxID       = "DISCOBOX_SANDBOX_ID"
	EnvWorkerID        = "DISCOBOX_WORKER_ID"
	EnvBootstrapToken  = "DISCOBOX_WORKER_BOOTSTRAP_TOKEN"
	EnvAgentPort       = "DISCOBOX_AGENT_PORT"
)

// Bootstrap is the VM boot contract used by the control plane and worker agent.
type Bootstrap struct {
	ControlPlaneURL string `json:"controlPlaneUrl,omitempty"`
	TenantID        string `json:"tenantId,omitempty"`
	ProjectID       string `json:"projectId,omitempty"`
	SandboxID       string `json:"sandboxId,omitempty"`
	WorkerID        string `json:"workerId,omitempty"`
	Token           string `json:"token,omitempty"`
	AgentPort       int    `json:"agentPort,omitempty"`
}

// Config controls worker registration.
type Config struct {
	Bootstrap Bootstrap
	Client    Client
	KeySource KeySource
}

// Client registers a booted worker with the control plane.
type Client interface {
	RegisterWorker(ctx context.Context, req RegisterRequest) (*RegisterResponse, error)
}

type StatusClient interface {
	UpdateWorkerStatus(ctx context.Context, req StatusRequest) error
}

// RegisterRequest is sent by the worker after generating its keypair.
type RegisterRequest struct {
	ControlPlaneURL string `json:"-"`
	TenantID        string `json:"tenantId"`
	ProjectID       string `json:"projectId"`
	SandboxID       string `json:"sandboxId"`
	WorkerID        string `json:"workerId"`
	BootstrapToken  string `json:"bootstrapToken"`
	PublicKey       string `json:"publicKey"`
	KeyType         string `json:"keyType"`
}

// StatusRequest updates worker scheduling status using the auth token returned
// by registration. AuthToken is sent as an Authorization: Bearer header.
type StatusRequest struct {
	ControlPlaneURL       string  `json:"-"`
	TenantID              string  `json:"tenantId"`
	WorkerID              string  `json:"workerId"`
	AuthToken             string  `json:"-"`
	Ready                 bool    `json:"ready"`
	Schedulable           bool    `json:"schedulable"`
	Degraded              bool    `json:"degraded"`
	AvailableCPUVCPUs     float64 `json:"availableCpuVcpus"`
	AvailableMemoryBytes  int64   `json:"availableMemoryBytes"`
	AvailableStorageBytes int64   `json:"availableStorageBytes"`
	Conditions            any     `json:"conditions,omitempty"`
}

// RegisterResponse is returned by the control plane after worker registration.
type RegisterResponse struct {
	AuthToken string `json:"authToken"`
}

// Registration is the completed worker startup identity state.
type Registration struct {
	Bootstrap  Bootstrap
	PublicKey  string
	PrivateKey ed25519.PrivateKey
	AuthToken  string
}

// KeySource generates or loads the worker identity keypair.
type KeySource interface {
	KeyPair(ctx context.Context) (publicKey string, privateKey ed25519.PrivateKey, err error)
}

// Run performs the startup registration flow once.
func Run(ctx context.Context, cfg Config) (*Registration, error) {
	bootstrap := cfg.Bootstrap
	if err := bootstrap.Validate(); err != nil {
		return nil, err
	}
	client := cfg.Client
	if client == nil {
		client = NewHTTPClient(bootstrap.ControlPlaneURL)
	}
	keySource := cfg.KeySource
	if keySource == nil {
		keySource = GenerateKeySource{}
	}
	publicKey, privateKey, err := keySource.KeyPair(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := client.RegisterWorker(ctx, RegisterRequest{
		ControlPlaneURL: bootstrap.ControlPlaneURL,
		TenantID:        bootstrap.TenantID,
		ProjectID:       bootstrap.ProjectID,
		SandboxID:       bootstrap.SandboxID,
		WorkerID:        bootstrap.WorkerID,
		BootstrapToken:  bootstrap.Token,
		PublicKey:       publicKey,
		KeyType:         "ed25519",
	})
	if err != nil {
		return nil, err
	}
	if resp == nil || strings.TrimSpace(resp.AuthToken) == "" {
		return nil, errors.New("worker registration did not return an auth token")
	}
	return &Registration{
		Bootstrap:  bootstrap,
		PublicKey:  publicKey,
		PrivateKey: privateKey,
		AuthToken:  resp.AuthToken,
	}, nil
}

// FromEnv builds Bootstrap from environment variables.
func FromEnv() Bootstrap {
	agentPort, _ := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvAgentPort)))
	return Bootstrap{
		ControlPlaneURL: strings.TrimSpace(os.Getenv(EnvControlPlaneURL)),
		TenantID:        strings.TrimSpace(os.Getenv(EnvTenantID)),
		ProjectID:       strings.TrimSpace(os.Getenv(EnvProjectID)),
		SandboxID:       strings.TrimSpace(os.Getenv(EnvSandboxID)),
		WorkerID:        strings.TrimSpace(os.Getenv(EnvWorkerID)),
		Token:           strings.TrimSpace(os.Getenv(EnvBootstrapToken)),
		AgentPort:       agentPort,
	}
}

func (b Bootstrap) Validate() error {
	if strings.TrimSpace(b.ControlPlaneURL) == "" {
		return errors.New("control plane URL is required")
	}
	if strings.TrimSpace(b.TenantID) == "" {
		return errors.New("tenant ID is required")
	}
	if strings.TrimSpace(b.WorkerID) == "" {
		return errors.New("worker ID is required")
	}
	if strings.TrimSpace(b.Token) == "" {
		return errors.New("worker bootstrap token is required")
	}
	return nil
}

// GenerateKeySource creates a fresh Ed25519 keypair.
type GenerateKeySource struct{}

func (GenerateKeySource) KeyPair(context.Context) (string, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, fmt.Errorf("generate worker keypair: %w", err)
	}
	return base64.StdEncoding.EncodeToString(pub), priv, nil
}
