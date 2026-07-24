// Package poolagent implements the in-guest pool startup registration flow.
package poolagent

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
	"github.com/obot-platform/discobox/pool-agent/poolauth"
	"github.com/obot-platform/discobox/pool-agent/vsock"
)

const (
	EnvControlPlaneURL = "DISCOBOX_CONTROL_PLANE_URL"
	EnvProjectID       = "DISCOBOX_PROJECT_ID"
	EnvPoolID          = "DISCOBOX_POOL_ID"
	EnvBootstrapToken  = "DISCOBOX_POOL_BOOTSTRAP_TOKEN" //nolint:gosec // Environment variable name, not a credential value.
	EnvControlPlaneKey = "DISCOBOX_CONTROL_PLANE_PUBLIC_KEY"
	EnvAgentPort       = "DISCOBOX_AGENT_PORT"
	EnvAgentVSOCKPort  = "DISCOBOX_AGENT_VSOCK_PORT"
	EnvHostMountPrefix = "DISCOBOX_POOL_HOST_MOUNT_PREFIX"
)

// Bootstrap is the VM boot contract used by the control plane and pool agent.
type Bootstrap struct {
	ControlPlaneURL string `json:"controlPlaneUrl,omitempty"`
	ProjectID       string `json:"projectId,omitempty"`
	PoolID          string `json:"poolId,omitempty"`
	Token           string `json:"token,omitempty"`
	ControlPlaneKey string `json:"controlPlanePublicKey,omitempty"`
	AgentPort       int    `json:"agentPort,omitempty"`
	// ControlPlaneVSOCKPort makes outbound control-plane HTTP dial host CID 2
	// over AF_VSOCK instead of the URL's IP transport.
	ControlPlaneVSOCKPort uint32 `json:"controlPlaneVsockPort,omitempty"`
	// AgentVSOCKPort makes the pool-agent HTTP server listen on AF_VSOCK
	// instead of TCP.
	AgentVSOCKPort  uint32 `json:"agentVsockPort,omitempty"`
	HostMountPrefix string `json:"hostMountPrefix,omitempty"`
}

// MintBootstrap produces the registration credentials for a pool runtime.
//
// Minting is a WRITE: it persists a single-use bootstrap token. Pool providers
// therefore take a mint function rather than a Bootstrap, and call it only when
// they actually create a runtime — a drift check that finds a healthy container
// needs no credentials and must not mint any. Minting eagerly on every reconcile
// leaks a token row per reconcile.
type MintBootstrap func(context.Context) (Bootstrap, error)

// Validate checks the required pool bootstrap fields.
func (b Bootstrap) Validate() error {
	if strings.TrimSpace(b.ControlPlaneURL) == "" {
		return errors.New("control plane URL is required")
	}
	if strings.TrimSpace(b.PoolID) == "" {
		return errors.New("pool ID is required")
	}
	if strings.TrimSpace(b.Token) == "" {
		return errors.New("pool bootstrap token is required")
	}
	if b.ControlPlaneVSOCKPort > 0 && b.ControlPlaneVSOCKPort < 1024 {
		return errors.New("control plane VSOCK port must be at least 1024")
	}
	if b.AgentVSOCKPort > 0 && b.AgentVSOCKPort < 1024 {
		return errors.New("agent VSOCK port must be at least 1024")
	}
	return nil
}

// Config controls pool registration.
type Config struct {
	Bootstrap Bootstrap
	Client    Client
	KeySource KeySource
}

// Client registers a booted pool with the control plane.
type Client interface {
	RegisterPool(ctx context.Context, req RegisterRequest) (*RegisterResponse, error)
}

type StatusClient interface {
	UpdatePoolStatus(ctx context.Context, req StatusRequest) error
}

type SandboxRemovalClient interface {
	ReportSandboxRemoved(ctx context.Context, req SandboxRemovalRequest) error
}

// RegisterRequest is sent by the pool after generating its keypair.
type RegisterRequest struct {
	ControlPlaneURL string `json:"-"`
	ProjectID       string `json:"projectId"`
	PoolID          string `json:"poolId,omitempty"`
	BootstrapToken  string `json:"bootstrapToken"`
	PublicKey       string `json:"publicKey"`
	KeyType         string `json:"keyType"`
}

// StatusRequest updates pool scheduling status using a signed pool assertion.
type StatusRequest struct {
	ControlPlaneURL       string             `json:"-"`
	ProjectID             string             `json:"-"`
	PoolID                string             `json:"-"`
	PrivateKey            ed25519.PrivateKey `json:"-"`
	Ready                 bool               `json:"ready"`
	Schedulable           bool               `json:"schedulable"`
	Degraded              bool               `json:"degraded"`
	AvailableCPUVCPUs     float64            `json:"availableCpuVcpus"`
	AvailableMemoryBytes  int64              `json:"availableMemoryBytes"`
	AvailableStorageBytes int64              `json:"availableStorageBytes"`
	Conditions            any                `json:"conditions,omitempty"`
}

// SandboxRemovalRequest reports a pool-local runtime removed outside the
// control plane's delete reconciliation.
type SandboxRemovalRequest struct {
	ControlPlaneURL string             `json:"-"`
	ProjectID       string             `json:"-"`
	PoolID          string             `json:"-"`
	PrivateKey      ed25519.PrivateKey `json:"-"`
	SandboxID       string             `json:"sandboxId"`
	// ContainerID is the runtime that was removed. The control plane ignores a
	// report naming a container it no longer believes is serving the sandbox, so
	// a report about an already-replaced container cannot countermand the
	// operation that replaced it (ADR 0016 §8). Empty when the report comes from
	// the level-triggered orphan sweep rather than a removal event.
	ContainerID string `json:"containerId,omitempty"`
}

// RegisterResponse is returned by the control plane after pool registration.
type RegisterResponse struct{}

// Registration is the completed pool startup identity state.
type Registration struct {
	Bootstrap  Bootstrap
	PublicKey  string
	PrivateKey ed25519.PrivateKey
}

// KeySource generates or loads the pool identity keypair.
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
	resp, err := client.RegisterPool(ctx, RegisterRequest{
		ControlPlaneURL: bootstrap.ControlPlaneURL,
		ProjectID:       bootstrap.ProjectID,
		PoolID:          bootstrap.PoolID,
		BootstrapToken:  bootstrap.Token,
		PublicKey:       publicKey,
		KeyType:         "ed25519",
	})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("pool registration did not return a response")
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
	agentVSOCKPort, _ := strconv.ParseUint(strings.TrimSpace(os.Getenv(EnvAgentVSOCKPort)), 10, 32)
	controlPlaneVSOCKPort, _ := strconv.ParseUint(strings.TrimSpace(os.Getenv(vsock.EnvControlPlanePort)), 10, 32)
	return Bootstrap{
		ControlPlaneURL:       controlPlaneURLFromEnv(),
		ProjectID:             strings.TrimSpace(os.Getenv(EnvProjectID)),
		PoolID:                strings.TrimSpace(os.Getenv(EnvPoolID)),
		Token:                 strings.TrimSpace(os.Getenv(EnvBootstrapToken)),
		ControlPlaneKey:       strings.TrimSpace(os.Getenv(EnvControlPlaneKey)),
		AgentPort:             agentPort,
		ControlPlaneVSOCKPort: uint32(controlPlaneVSOCKPort),
		AgentVSOCKPort:        uint32(agentVSOCKPort),
		HostMountPrefix:       strings.TrimSpace(os.Getenv(EnvHostMountPrefix)),
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
		return "", nil, fmt.Errorf("generate pool keypair: %w", err)
	}
	publicKey, err := poolauth.EncodePublicKey(pub)
	if err != nil {
		return "", nil, err
	}
	return publicKey, priv, nil
}
