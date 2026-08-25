package sandbox

import (
	"context"
	"io"
	"time"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/transport"
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

	// Archive tears the sandbox's runtime down and keeps its data: no container
	// and no runtime resources, but whatever durable state the provider holds
	// for it survives, so a later Create reinstantiates the same sandbox rather
	// than a fresh one (ADR 0022 §6).
	Archive(ctx context.Context, ref SandboxRef, state []byte) ([]byte, error)
	// Remove destroys the sandbox and its data, and returns only once the
	// provider has confirmed both are gone. The control plane deletes its row on
	// the strength of that return, so a provider that cannot confirm must error
	// rather than report success (ADR 0022 §3). Keeping the data is Archive's
	// job; there is no option here for it.
	Remove(ctx context.Context, ref SandboxRef, state []byte) ([]byte, error)
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
	UserGroupName         *string
	UserAdditionalGroups  []string
	HomeDirectory         *string
	GitUserName           *string
	GitUserEmail          *string
	ResolvedHarnessConfig *ResolvedHarnessConfig
	AgentServerURL        string
	OAuthRedirectBase     string
	PoolID                string
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
	ID              string
	Name            string
	Description     string
	RunCommand      []string
	RelaunchCommand []string
	ConfigCommand   []string
	Files           []model.HarnessConfigFile
	// ConfiguredFiles overlays Files by path (docs/adr/0012 §1's Files
	// overlay rule): files captured by the harness's configure flow, kept
	// separate from the image baseline so a reconfigure never has to
	// reconstruct or duplicate it.
	ConfiguredFiles []model.HarnessConfigFile
	// Secrets are the harness's declared credentials, carried for their
	// Delivery: a file-delivered secret must not also be exported into the
	// harness's environment (harness.SecretDeliveryFile).
	Secrets          []model.HarnessConfigSecret
	Env              map[string]string
	Volumes          []harness.Volume
	AdditionalGroups []string
}

// PoolRuntime is the provider surface for a pool's own runtime host: the pool
// is its own runtime host, so these converge and operate one container/VM/pod.
// The caller owns pool lifecycle persistence and job semantics. RepairPool is
// only for preserving in-place repair of pools with assigned sandboxes;
// delete reconciliation must use RemovePool and must not fall back to repair.
type PoolRuntime interface {
	ReconcilePool(ctx context.Context, manager PoolManager, project *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool) error
	RepairPool(ctx context.Context, manager PoolManager, project *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool, reason string) error
	RemovePool(ctx context.Context, manager PoolManager, project *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool) error
	// PreloadImages brings the pool up and pulls the images a sandbox will
	// want, so the first sandbox does not wait for either.
	PreloadImages(ctx context.Context, manager PoolManager, project *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool, images []string, report func(image string, done, total int)) error
	// OpenConsole attaches to the pool host's administrative console: a root
	// shell in the host's own namespaces, for operators debugging the backend
	// itself. It deliberately does not go through the pool agent, because the
	// agent is one of the things a broken host stops running.
	OpenConsole(ctx context.Context, provider *model.SandboxProviderInstance, pool *model.Pool, opts ConsoleOptions) (PTY, error)
}

// ConsoleOptions configures one attach to a pool host console.
//
// It carries only the terminal size, because the console session outlives any
// one attach: the container is created once per pool host and reattached, so
// anything baked in at creation would be whatever the first caller happened to
// ask for. Size is a property of the TTY and is applied on every attach.
type ConsoleOptions struct {
	Rows int
	Cols int
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
