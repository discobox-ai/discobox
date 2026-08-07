// Package poolagent implements the in-guest pool startup registration flow.
package poolagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/obot-platform/discobox/controlplane"
	"github.com/obot-platform/discobox/pool-agent/endpoint"
	"github.com/obot-platform/discobox/pool-agent/poolauth"
)

const (
	EnvControlPlaneURL = "DISCOBOX_CONTROL_PLANE_URL"
	EnvProjectID       = "DISCOBOX_PROJECT_ID"
	EnvPoolID          = "DISCOBOX_POOL_ID"
	EnvBootstrapToken  = "DISCOBOX_POOL_BOOTSTRAP_TOKEN" //nolint:gosec // Environment variable name, not a credential value.
	EnvControlPlaneKey = "DISCOBOX_CONTROL_PLANE_PUBLIC_KEY"
	// EnvAgentListenURL is the transport URL the pool-agent HTTP server binds.
	// The scheme selects the transport, so a backend that terminates the agent
	// API on VSOCK or a Unix socket needs no separate port variable.
	EnvAgentListenURL  = "DISCOBOX_AGENT_LISTEN_URL"
	EnvHostMountPrefix = "DISCOBOX_POOL_HOST_MOUNT_PREFIX"
	// EnvHostStateRoot is where the Docker daemon sees layout.ContainerRoot. It
	// is empty when the daemon sees the same path the container does; a backend
	// sets it when it must place state elsewhere, as wslc does to reach the only
	// disk it persists.
	EnvHostStateRoot = "DISCOBOX_POOL_HOST_STATE_ROOT"
)

// Bootstrap is the VM boot contract used by the control plane and pool agent.
//
// Both directions are addressed by a single URL each, and the scheme alone
// decides the transport (see pool-agent/endpoint). A backend is therefore
// expressed entirely in the URLs it renders — http:// for a pool that shares the
// host network, vsock://2:3001 for a libkrun microVM, unix:///... for a guest
// whose helper terminates the socket — with no transport-specific field here.
type Bootstrap struct {
	// ControlPlaneURL is where the agent reaches the control plane.
	ControlPlaneURL string `json:"controlPlaneUrl,omitempty"`
	ProjectID       string `json:"projectId,omitempty"`
	PoolID          string `json:"poolId,omitempty"`
	Token           string `json:"token,omitempty"`
	ControlPlaneKey string `json:"controlPlanePublicKey,omitempty"`
	// AgentListenURL is where the agent's own HTTP server binds.
	AgentListenURL  string `json:"agentListenUrl,omitempty"`
	HostMountPrefix string `json:"hostMountPrefix,omitempty"`
	// HostStateRoot is where the pool's Docker daemon sees layout.ContainerRoot.
	// Empty means no relocation. Only paths handed to the daemon are translated;
	// everything the agent reads and writes stays in container terms.
	HostStateRoot string `json:"hostStateRoot,omitempty"`
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
	// Validating here means an unreachable transport is rejected at boot with a
	// clear message, rather than at the first request.
	if _, err := endpoint.Parse(b.ControlPlaneURL); err != nil {
		return fmt.Errorf("control plane URL: %w", err)
	}
	if listen := strings.TrimSpace(b.AgentListenURL); listen != "" {
		if _, err := endpoint.Parse(listen); err != nil {
			return fmt.Errorf("agent listen URL: %w", err)
		}
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

// SandboxStateClient publishes observed sandbox states to the control plane.
// It is the channel that makes power state knowable at all: operations answer
// with acceptance only, and the transitions that matter most — a container
// dying, a host rebooting — have no request to answer (ADR 0017 §10).
type SandboxStateClient interface {
	ReportSandboxStates(ctx context.Context, req SandboxStateRequest) error
}

// SandboxAgentStatusClient mints scoped sandbox-agent tokens and pushes
// polled sandbox-agent status batches to the control plane (ADR 0030).
type SandboxAgentStatusClient interface {
	MintSandboxAgentStatusTokens(ctx context.Context, req MintSandboxAgentStatusTokensRequest) (*MintSandboxAgentStatusTokensResponse, error)
	ReportSandboxAgentStatus(ctx context.Context, req SandboxAgentStatusReportRequest) error
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

// SandboxState is one observation about one sandbox.
type SandboxState struct {
	SandboxID string `json:"sandboxId"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
}

// SandboxStateRequest is one delivery on the state channel.
type SandboxStateRequest struct {
	ControlPlaneURL string             `json:"-"`
	ProjectID       string             `json:"-"`
	PoolID          string             `json:"-"`
	PrivateKey      ed25519.PrivateKey `json:"-"`

	// BootID identifies this run of the agent and Sequence counts batches
	// within it. Together they let the control plane drop a delayed delta that
	// would otherwise overwrite a newer full sync; across boots the sequence
	// restarts and means nothing, so ReportedAt decides.
	BootID     string    `json:"bootId"`
	Sequence   int64     `json:"sequence"`
	ReportedAt time.Time `json:"reportedAt"`
	// Complete marks the periodic full sync: States is every sandbox this agent
	// hosts, so a sandbox the control plane thinks is here and that States omits
	// no longer has a container.
	Complete bool           `json:"complete"`
	States   []SandboxState `json:"states"`
}

// MintSandboxAgentStatusTokensRequest asks the control plane for short-lived,
// status:read-only sandbox-agent tokens for the sandboxes this pool hosts.
// It carries no scope: the control plane always mints status:read only,
// regardless of what is requested.
type MintSandboxAgentStatusTokensRequest struct {
	ControlPlaneURL string             `json:"-"`
	ProjectID       string             `json:"-"`
	PoolID          string             `json:"-"`
	PrivateKey      ed25519.PrivateKey `json:"-"`
	SandboxIDs      []string           `json:"sandboxIds"`
}

// SandboxAgentStatusToken is one minted, status:read-only sandbox-agent
// token.
type SandboxAgentStatusToken struct {
	SandboxID string    `json:"sandboxId"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// MintSandboxAgentStatusTokensResponse is the control plane's response to a
// token mint request.
type MintSandboxAgentStatusTokensResponse struct {
	Tokens []SandboxAgentStatusToken `json:"tokens"`
}

// SandboxAgentStatusEntry is one sandbox's polled status, relayed to the
// control plane opaquely (pool-agent never parses Status).
type SandboxAgentStatusEntry struct {
	SandboxID  string          `json:"sandboxId"`
	Status     json.RawMessage `json:"status"`
	ObservedAt time.Time       `json:"observedAt"`
}

// SandboxAgentStatusReportRequest pushes this tick's successfully polled
// sandbox-agent status batch to the control plane. Sandboxes that failed to
// poll this tick are omitted, not reported as stale.
type SandboxAgentStatusReportRequest struct {
	ControlPlaneURL string                    `json:"-"`
	ProjectID       string                    `json:"-"`
	PoolID          string                    `json:"-"`
	PrivateKey      ed25519.PrivateKey        `json:"-"`
	Sandboxes       []SandboxAgentStatusEntry `json:"sandboxes"`
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
	return Bootstrap{
		ControlPlaneURL: controlPlaneURLFromEnv(),
		ProjectID:       strings.TrimSpace(os.Getenv(EnvProjectID)),
		PoolID:          strings.TrimSpace(os.Getenv(EnvPoolID)),
		Token:           strings.TrimSpace(os.Getenv(EnvBootstrapToken)),
		ControlPlaneKey: strings.TrimSpace(os.Getenv(EnvControlPlaneKey)),
		AgentListenURL:  strings.TrimSpace(os.Getenv(EnvAgentListenURL)),
		HostMountPrefix: strings.TrimSpace(os.Getenv(EnvHostMountPrefix)),
		HostStateRoot:   strings.TrimSpace(os.Getenv(EnvHostStateRoot)),
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
