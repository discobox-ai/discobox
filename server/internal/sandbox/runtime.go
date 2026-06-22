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

	List(ctx context.Context) ([]*Sandbox, error)

	Create(ctx context.Context, ref SandboxRef, state []byte, opts CreateOptions) (*Sandbox, []byte, error)
	Start(ctx context.Context, ref SandboxRef, state []byte) (*Sandbox, []byte, error)
	Stop(ctx context.Context, ref SandboxRef, state []byte, timeout time.Duration) (*Sandbox, []byte, error)
	Remove(ctx context.Context, ref SandboxRef, state []byte, opts ...RemoveOption) ([]byte, error)
	Get(ctx context.Context, ref SandboxRef, state []byte) (*Sandbox, error)
	AcquireHTTPClient(ctx context.Context, ref SandboxRef, state []byte) (*transport.HTTPClientLease, error)
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

	Name                     string
	Description              *string
	AgentConfigID            *string
	AgentModel               *string
	AgentModelServiceTier    *string
	AgentModelReasoningLevel *string
	Prompt                   *string
	SourceURL                string
	SourceRef                string
	SourceRefType            string
	SourceDirectory          string
	SourceCodeReferences     model.SourceCodeReferences
	UserUID                  *int
	UserGID                  *int
	WorkspacePath            string
	WorkspaceSource          string
	WorkspaceRef             string
	WorkingDirectory         string
	AgentServerURL           string
	OAuthRedirectBase        string
	Resources                ResourceConfig
	ProviderInstanceID       string
	WorkerID                 string
	CPUVCPUs                 float64
	MemoryBytes              int64
	StorageBytes             int64
}

// PrepareStateProvider can precompute provider-owned state before creation.
type PrepareStateProvider interface {
	PrepareState(ctx context.Context, ref SandboxRef, opts CreateOptions) ([]byte, error)
}

// WatchProvider can report provider runtime state changes.
type WatchProvider interface {
	Watch(ctx context.Context) (<-chan StateEvent, error)
}

// ReconcileProvider can repair provider runtime state after process startup.
type ReconcileProvider interface {
	Reconcile(ctx context.Context) error
}

// WorkerProviderReconciler reconciles worker-provider state for a provider
// instance, such as maintaining a warm worker pool.
type WorkerProviderReconciler interface {
	ReconcileWorkerProvider(ctx context.Context, store any, project *model.Project, provider *model.SandboxProviderInstance) error
}

// WorkerRuntimeReconciler reconciles provider-owned runtime state for a worker
// resource. The caller owns worker lifecycle persistence and job semantics.
type WorkerRuntimeReconciler interface {
	ReconcileWorker(ctx context.Context, store any, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker) error
	RemoveWorker(ctx context.Context, store any, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker) error
}

// ProjectRemover can remove all provider-managed resources for a project.
type ProjectRemover interface {
	RemoveProject(ctx context.Context, projectID string) error
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

type HTTPClientLease = transport.HTTPClientLease

// NewHTTPClientLease creates a lease around a client and release callback.
var NewHTTPClientLease = transport.NewHTTPClientLease

// NewHTTPClientLeaseWithBaseURL creates a lease with a preferred logical base URL.
var NewHTTPClientLeaseWithBaseURL = transport.NewHTTPClientLeaseWithBaseURL

// NewHTTPClientLeaseWithBaseURLAndAuth creates a lease with a base URL and bearer token.
var NewHTTPClientLeaseWithBaseURLAndAuth = transport.NewHTTPClientLeaseWithBaseURLAndAuth

// NewHTTPClientLeaseWithAuth creates an authenticated lease for a client that
// handles the logical worker URL itself, for example by dialing a VS Code socket
// or Unix socket from a custom RoundTripper.
var NewHTTPClientLeaseWithAuth = transport.NewHTTPClientLeaseWithAuth
