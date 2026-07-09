package sandbox

import (
	"context"
	"io"
	"time"

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
	Start(ctx context.Context, ref SandboxRef, state []byte) (*Sandbox, []byte, error)
	Stop(ctx context.Context, ref SandboxRef, state []byte, timeout time.Duration) (*Sandbox, []byte, error)
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

	Labels map[string]string
	Env    map[string]string
	// Sentinels are the placeholder secret values injected into the sandbox that
	// the worker registers with the proxy for runtime swapping.
	Sentinels []string

	Name                     string
	Description              *string
	AgentConfigID            *string
	AgentModel               *string
	AgentModelServiceTier    *string
	AgentModelReasoningLevel *string
	Prompt                   []string
	Source                   *model.GitSource
	SourceCodeReferences     model.SourceCodeReferences
	UserName                 *string
	UserUID                  *int
	UserGID                  *int
	HomeDirectory            *string
	ResolvedAgentConfig      *ResolvedAgentConfig
	AgentConfigs             []AgentConfig
	AgentServerURL           string
	OAuthRedirectBase        string
	Resources                ResourceConfig
	ProviderInstanceID       string
	WorkerID                 string
	CPUVCPUs                 float64
	MemoryBytes              int64
	StorageBytes             int64
}

// ResolvedAgentConfig is the sandbox-local coding agent configuration captured
// at sandbox create time.
type ResolvedAgentConfig struct {
	ID              string
	Name            string
	InstallCommand  []string
	RunCommand      []string
	RelaunchCommand []string
	Files           []model.AgentConfigFile
}

// AgentConfig is a project-scoped coding agent configuration made available to
// the sandbox runtime.
type AgentConfig struct {
	ID              string
	Name            string
	InstallCommand  []string
	RunCommand      []string
	RelaunchCommand []string
	IsDefault       bool
	Files           []model.AgentConfigFile
}

// WorkerProviderReconciler reconciles worker-provider state for a provider
// instance, such as maintaining a worker pool.
type WorkerProviderReconciler interface {
	ReconcileWorkerProvider(ctx context.Context, manager WorkerManager, project *model.Project, provider *model.SandboxProviderInstance) error
}

// WorkerRuntimeReconciler reconciles provider-owned runtime state for a worker
// resource. The caller owns worker lifecycle persistence and job semantics.
// RepairWorker is only for preserving repair of active assigned workers; delete
// reconciliation must use RemoveWorker and must not fall back to repair.
type WorkerRuntimeReconciler interface {
	ReconcileWorker(ctx context.Context, manager WorkerManager, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker) error
	RepairWorker(ctx context.Context, manager WorkerManager, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker, reason string) error
	RemoveWorker(ctx context.Context, manager WorkerManager, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker) error
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
