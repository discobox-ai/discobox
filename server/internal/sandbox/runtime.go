package sandbox

import (
	"context"
	"io"
	"time"

	"github.com/obot-platform/discobox/harness"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/transport"
)

// Provider abstracts sandbox runtime environments.
//
// Providers own runtime mechanics only. Application services own persistence,
// orchestration, authorization, and API shape.
type Provider interface {
	Initialize(ctx context.Context, instance *model.SandboxProviderInstance) error
	Close() error
	Definition() ProviderDefinition
	Status() ProviderStatus
	Reconcile(ctx context.Context) error
	RemoveProject(ctx context.Context, projectID string) error

	List(ctx context.Context) ([]*Sandbox, error)

	Create(ctx context.Context, ref SandboxRef, state []byte, opts CreateOptions) (*Sandbox, []byte, error)
	// Update applies the mutable subset of a sandbox's configuration to a running
	// instance in place. Only the fields present in UpdateOptions can change.
	Update(ctx context.Context, ref SandboxRef, state []byte, opts UpdateOptions) (*Sandbox, []byte, error)

	// Start, Stop, and Restart instruct the runtime and return only whether the
	// instruction was accepted. They deliberately do not return a Sandbox: the
	// resulting state arrives on the runtime's own reporting channel, because a
	// response cannot express "starting" and because the transitions that matter
	// most — a container dying, a host rebooting — have no request to answer
	// (ADR 0017 §§9–10).
	Start(ctx context.Context, ref SandboxRef, state []byte) ([]byte, error)
	Stop(ctx context.Context, ref SandboxRef, state []byte, timeout time.Duration) ([]byte, error)
	Restart(ctx context.Context, ref SandboxRef, state []byte, timeout time.Duration) ([]byte, error)

	Remove(ctx context.Context, ref SandboxRef, state []byte, opts ...RemoveOption) ([]byte, error)
	Get(ctx context.Context, ref SandboxRef, state []byte) (*Sandbox, error)
	AcquireHTTPClient(ctx context.Context, ref SandboxRef, state []byte, scopes []string) (*transport.HTTPClientLease, error)
}

// SandboxRef identifies the sandbox and its project ownership context.
//
// ProjectID is required because many providers use project scope for placement,
// shared caches, VM selection, resource settings, and cleanup.
type SandboxRef struct {
	SandboxID string
	ProjectID string
}

// Sandbox is the runtime provider's view of a sandbox instance.
type Sandbox struct {
	ID        string
	SandboxID string
	Status    Status
	Image     string
	CreatedAt time.Time
	StartedAt *time.Time
	StoppedAt *time.Time
	Error     string
	Metadata  map[string]string
	Ports     []AssignedPort
	Env       map[string]string
}

// AssignedPort describes a runtime-assigned port mapping.
type AssignedPort struct {
	ContainerPort int
	HostPort      int
	HostIP        string
	Protocol      string
}

// Status is the provider runtime status.
type Status string

const (
	StatusCreated Status = "created"
	StatusRunning Status = "running"
	StatusStopped Status = "stopped"
	StatusFailed  Status = "failed"
	StatusRemoved Status = "removed"
)

// StateEvent reports a provider runtime state change.
type StateEvent struct {
	SandboxID  string
	Status     Status
	Timestamp  time.Time
	Error      string
	ProviderID string
}

// CreateOptions configures sandbox creation.
type CreateOptions struct {
	Image ImageRef
	// SpecFingerprint is the digest of the sandbox's whole manifest. The
	// runtime records it on the container it builds and rebuilds any container
	// whose recorded fingerprint no longer matches, which is how every spec
	// change — image, resources, sources, whatever is added later — converges
	// through one mechanism (ADR 0017 §5).
	SpecFingerprint string
	// Start asks the runtime to bring the container up as part of creating it.
	// True only for a sandbox that has never run; a rebuild after the container
	// was lost restores it stopped (ADR 0017 §13).
	Start bool

	Labels map[string]string
	Env    map[string]string
	// Sentinels are the placeholder secret values injected into the sandbox that
	// the worker registers with the proxy for runtime swapping.
	Sentinels []string
	// SecretEnv maps each secret-bound environment variable name to its
	// sentinel placeholder value. Unlike Env, these never ride in the static
	// sandbox.json document (docs/adr/0012 §3) — the provider delivers them
	// through a separate, independently-refreshed channel
	// (/run/discobox/secrets/secrets.json) so a resolved sentinel value can
	// change (rotation, grant approval, OAuth refresh) without touching the
	// sandbox's static config.
	SecretEnv map[string]string

	Name                  string
	Description           *string
	HarnessConfigID       *string
	HarnessMode           string
	Model                 *string
	ModelServiceTier      *string
	ModelReasoningLevel   *string
	Prompt                []string
	Source                *model.GitSource
	SourceCodeReferences  model.SourceCodeReferences
	UserName              *string
	UserUID               *int
	UserGID               *int
	HomeDirectory         *string
	ResolvedHarnessConfig *ResolvedHarnessConfig
	AgentServerURL        string
	OAuthRedirectBase     string
	Resources             ResourceConfig
	PoolID                string
	CPUVCPUs              float64
	MemoryBytes           int64
	StorageBytes          int64
}

// UpdateOptions carries the mutable subset of CreateOptions that can be applied
// to a running sandbox in place. It mirrors the CreateOptions shape; only the
// fields present here may be updated after creation.
type UpdateOptions struct {
	// Sentinels replaces the placeholder secret set registered with the proxy for
	// runtime swapping. It mirrors CreateOptions.Sentinels.
	Sentinels []string
	// SecretEnv replaces the sandbox's secret-bound env->sentinel map. It
	// mirrors CreateOptions.SecretEnv.
	SecretEnv map[string]string
}

// ResolvedHarnessConfig is the sandbox-local harness configuration captured
// at sandbox create time.
type ResolvedHarnessConfig struct {
	ID               string
	Name             string
	Description      string
	RunCommand       []string
	RelaunchCommand  []string
	ConfigCommand    []string
	Files            []model.HarnessConfigFile
	Env              map[string]string
	Volumes          []harness.Volume
	AdditionalGroups []string
}

// PoolRuntimeReconciler reconciles provider-owned runtime state for a Pool:
// the pool is its own runtime host, so these converge one container/VM/pod.
// The caller owns pool lifecycle persistence and job semantics. RepairPool is
// only for preserving in-place repair of pools with assigned sandboxes;
// delete reconciliation must use RemovePool and must not fall back to repair.
type PoolRuntimeReconciler interface {
	ReconcilePool(ctx context.Context, manager PoolManager, project *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool) error
	RepairPool(ctx context.Context, manager PoolManager, project *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool, reason string) error
	RemovePool(ctx context.Context, manager PoolManager, project *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool) error
}

// ResourceConfig defines runtime resource limits.
type ResourceConfig struct {
	MemoryMB int
	CPUCores float64
	DiskMB   int
	Timeout  time.Duration
}

// RemoveOption configures sandbox removal behavior.
type RemoveOption func(*RemoveConfig)

// RemoveConfig holds parsed remove options.
type RemoveConfig struct {
	RemoveVolumes bool
}

// RemoveVolumes enables provider-managed volume/data deletion.
func RemoveVolumes() RemoveOption {
	return func(cfg *RemoveConfig) {
		cfg.RemoveVolumes = true
	}
}

// ParseRemoveOptions applies remove options to defaults.
func ParseRemoveOptions(opts []RemoveOption) RemoveConfig {
	cfg := RemoveConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

// AttachOptions configures an interactive PTY.
type AttachOptions struct {
	Cmd     []string
	Rows    int
	Cols    int
	WorkDir string
	Env     map[string]string
	User    string
}

// PTY is an interactive terminal session.
type PTY interface {
	io.ReadWriteCloser
	Resize(ctx context.Context, rows, cols int) error
	Wait(ctx context.Context) (int, error)
}

// ExecStreamOptions configures streaming command execution.
type ExecStreamOptions struct {
	WorkDir string
	Env     map[string]string
	User    string
	TTY     bool
}

// Stream is a bidirectional command stream.
type Stream interface {
	io.ReadWriteCloser
	Stderr() io.Reader
	Resize(ctx context.Context, rows, cols int) error
	CloseWrite() error
	Wait(ctx context.Context) (int, error)
}
