// Package workeragent implements the in-guest worker startup registration flow.
package workeragent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/obot-platform/discobox/controlplane"
	"github.com/obot-platform/discobox/worker-agent/workerauth"
)

const (
	EnvControlPlaneURL = "DISCOBOX_CONTROL_PLANE_URL"
	EnvProjectID       = "DISCOBOX_PROJECT_ID"
	EnvSandboxID       = "DISCOBOX_SANDBOX_ID"
	EnvWorkerID        = "DISCOBOX_WORKER_ID"
	EnvBootstrapToken  = "DISCOBOX_WORKER_BOOTSTRAP_TOKEN" //nolint:gosec // Environment variable name, not a credential value.
	EnvControlPlaneKey = "DISCOBOX_CONTROL_PLANE_PUBLIC_KEY"
	EnvAgentPort       = "DISCOBOX_AGENT_PORT"
)

// Bootstrap is the VM boot contract used by the control plane and worker agent.
type Bootstrap struct {
	ControlPlaneURL string `json:"controlPlaneUrl,omitempty"`
	ProjectID       string `json:"projectId,omitempty"`
	SandboxID       string `json:"sandboxId,omitempty"`
	WorkerID        string `json:"workerId,omitempty"`
	Token           string `json:"token,omitempty"`
	ControlPlaneKey string `json:"controlPlanePublicKey,omitempty"`
	AgentPort       int    `json:"agentPort,omitempty"`
}

// Validate checks the required worker bootstrap fields.
func (b Bootstrap) Validate() error {
	if strings.TrimSpace(b.ControlPlaneURL) == "" {
		return errors.New("control plane URL is required")
	}
	if strings.TrimSpace(b.WorkerID) == "" {
		return errors.New("worker ID is required")
	}
	if strings.TrimSpace(b.Token) == "" {
		return errors.New("worker bootstrap token is required")
	}
	return nil
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
	ProjectID       string `json:"projectId"`
	SandboxID       string `json:"sandboxId"`
	WorkerID        string `json:"workerId,omitempty"`
	BootstrapToken  string `json:"bootstrapToken"`
	PublicKey       string `json:"publicKey"`
	KeyType         string `json:"keyType"`
}

// StatusRequest updates worker scheduling status using a signed worker assertion.
type StatusRequest struct {
	ControlPlaneURL       string             `json:"-"`
	ProjectID             string             `json:"-"`
	WorkerID              string             `json:"-"`
	PrivateKey            ed25519.PrivateKey `json:"-"`
	Ready                 bool               `json:"ready"`
	Schedulable           bool               `json:"schedulable"`
	Degraded              bool               `json:"degraded"`
	AvailableCPUVCPUs     float64            `json:"availableCpuVcpus"`
	AvailableMemoryBytes  int64              `json:"availableMemoryBytes"`
	AvailableStorageBytes int64              `json:"availableStorageBytes"`
	Conditions            any                `json:"conditions,omitempty"`
}

// RegisterResponse is returned by the control plane after worker registration.
type RegisterResponse struct{}

// Registration is the completed worker startup identity state.
type Registration struct {
	Bootstrap  Bootstrap
	PublicKey  string
	PrivateKey ed25519.PrivateKey
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
	if resp == nil {
		return nil, errors.New("worker registration did not return a response")
	}
	return &Registration{
		Bootstrap:  bootstrap,
		PublicKey:  publicKey,
		PrivateKey: privateKey,
	}, nil
}

// FromEnv builds Bootstrap from environment variables.
func FromEnv() Bootstrap {
	agentPort, _ := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvAgentPort)))
	return Bootstrap{
		ControlPlaneURL: controlPlaneURLFromEnv(),
		ProjectID:       strings.TrimSpace(os.Getenv(EnvProjectID)),
		SandboxID:       strings.TrimSpace(os.Getenv(EnvSandboxID)),
		WorkerID:        strings.TrimSpace(os.Getenv(EnvWorkerID)),
		Token:           strings.TrimSpace(os.Getenv(EnvBootstrapToken)),
		ControlPlaneKey: strings.TrimSpace(os.Getenv(EnvControlPlaneKey)),
		AgentPort:       agentPort,
	}
}

func controlPlaneURLFromEnv() string {
	if value := strings.TrimSpace(os.Getenv(EnvControlPlaneURL)); value != "" {
		return value
	}
	return controlplane.DefaultURL("localhost", controlplane.DefaultPort)
}

// GenerateKeySource creates a fresh Ed25519 keypair.
type GenerateKeySource struct{}

func (GenerateKeySource) KeyPair(context.Context) (string, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, fmt.Errorf("generate worker keypair: %w", err)
	}
	publicKey, err := workerauth.EncodePublicKey(pub)
	if err != nil {
		return "", nil, err
	}
	return publicKey, priv, nil
}
