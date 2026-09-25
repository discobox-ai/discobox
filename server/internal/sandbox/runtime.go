package sandbox

import (
	"context"
	"io"
	"time"

	"github.com/discobox-ai/discobox/auditid"
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

	// ExportTree streams the sandbox's durable tree -- the data, workspace, and
	// push-delivered origin repositories that survive a container rebuild -- as
	// a tar archive (ADR 0123 §1). It refuses a running sandbox, because a tar
	// of a live tree is not a consistent one.
	//
	// Like ImportTree it is told the pool rather than reading one out of runtime
	// state. A sandbox whose create never got as far as a runtime has no state
	// to read and still has a tree on the pool its row names -- and is exactly
	// the sandbox somebody wants to export, because it is broken where it is.
	//
	// The stream is the caller's to close, and the walk behind it runs while
	// they read, so a failure part way through arrives as a read error rather
	// than as this call's.
	//
	// image is the sandbox's pin. The tree's `data` and `sources` are read by
	// the sandbox agent's export mode, from the sandbox's own image, and an
	// archived sandbox has no container left to name it (ADR 0129 §1).
	ExportTree(ctx context.Context, ref SandboxRef, poolID string, image ImageRef, state []byte) (io.ReadCloser, error)
	// ImportTree restores a durable tree onto a pool for a sandbox that has no
	// runtime and, at this point, no row either (ADR 0123 §3). The pool is named
	// rather than read from runtime state for exactly that reason: there is no
	// state yet to read it from. It returns the pool the tree landed on, which
	// is what the caller records on the sandbox it then creates.
	//
	// It writes the tree and nothing else. What it leaves behind is the shape
	// an archived sandbox has, so the ordinary create that follows adopts it
	// the way an unarchive does.
	ImportTree(ctx context.Context, ref SandboxRef, poolID string, tree io.Reader) (string, error)
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
	// the pool agent registers with the proxy for runtime swapping.
	Sentinels []string
	// SecretEnv maps each secret-bound environment variable name to its
	// sentinel placeholder value. Unlike Env, these never ride in the static
	// sandbox.json document (docs/adr/0012 §3) — the provider delivers them
	// through a separate, independently-refreshed channel
	// (/run/discobox/secrets/secrets.json) so a resolved sentinel value can
	// change (rotation, grant approval, OAuth refresh) without touching the
	// sandbox's static config.
	SecretEnv map[string]string

	Name                 string
	Description          *string
	HarnessConfigID      *string
	HarnessMode          string
	Model                *string
	ModelServiceTier     *string
	ModelReasoningLevel  *string
	Prompt               []string
	Source               *model.GitSource
	SourceCodeReferences model.SourceCodeReferences
	// SourceDataKey and SourceCodeReferenceDataKeys identify the durable,
	// pool-local data directory shared by sandboxes that use the same source.
	// The provider forwards these opaque keys; it does not derive source
	// identity itself.
	SourceDataKey               string
	SourceCodeReferenceDataKeys map[string]string
	UserName                    *string
	UserUID                     *int
	UserGID                     *int
	UserGroupName               *string
	UserAdditionalGroups        []string
	HomeDirectory               *string
	GitUserName                 *string
	GitUserEmail                *string
	ResolvedHarnessConfig       *ResolvedHarnessConfig
	AgentServerURL              string
	OAuthRedirectBase           string
	PoolID                      string
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

// HTTPAuditQuery narrows a read of one pool proxy's HTTP audit.
type HTTPAuditQuery struct {
	SandboxID string
	Host      string
	UseID     string
	Since     time.Time
	// Until keeps records written at or before it, for a reader paging back
	// newest first.
	Until time.Time
	// MinStatus and MaxStatus bound the response status, inclusive; zero
	// leaves that side open.
	MinStatus int
	MaxStatus int
	// Blocked selects refused exchanges when true and admitted ones when
	// false; nil is both.
	Blocked *bool
	// Ascending reads oldest first, for a follower reading forward from Since.
	Ascending bool
	// AfterID reads the records this pool wrote after one already read, in
	// write order, and takes precedence over Since and Until: it is the cursor a follower
	// of this pool's trail uses. IDs are per pool, so it is only meaningful
	// with the pool it came from.
	AfterID auditid.ExchangeID
	Limit   int
}

// DNSAuditFilter narrows a read of the DNS queries one pool answered for its
// sandboxes (ADR 0148). Its fields mean what HTTPAuditQuery's do; Name is the
// name asked, matched exactly.
type DNSAuditFilter struct {
	// ID reads the one query with this ID on the pool asked.
	ID        auditid.DNSQueryID
	SandboxID string
	Name      string
	Since     time.Time
	Until     time.Time
	Ascending bool
	AfterID   auditid.DNSQueryID
	Limit     int
}

// DNSAuditQuery is one DNS query a pool answered and audited: the question,
// and the response code and answers' data, or why there was none.
type DNSAuditQuery struct {
	ID             auditid.DNSQueryID `json:"id"`
	CreatedAt      time.Time          `json:"createdAt"`
	SandboxID      string             `json:"sandboxId"`
	Name           string             `json:"name"`
	Type           string             `json:"type"`
	RCode          string             `json:"rcode"`
	Answers        []string           `json:"answers"`
	DurationMillis int64              `json:"durationMillis"`
	Error          string             `json:"error,omitempty"`
}

// HTTPAuditArtifact is a body or upgraded stream recorded beside an audited
// exchange, still being read. Closing it releases whatever reaches the pool.
type HTTPAuditArtifact struct {
	Body        io.ReadCloser
	Format      string
	ContentType string
}

// HTTPAuditExchange is one HTTP exchange a pool proxy audited. Headers and
// bodies stay on the pool.
type HTTPAuditExchange struct {
	ID               auditid.ExchangeID `json:"id"`
	CreatedAt        time.Time          `json:"createdAt"`
	SandboxID        string             `json:"sandboxId"`
	Method           string             `json:"method"`
	URL              string             `json:"url"`
	Host             string             `json:"host"`
	Status           int                `json:"status"`
	DurationMillis   int64              `json:"durationMillis"`
	Blocked          bool               `json:"blocked"`
	BlockedReason    string             `json:"blockedReason,omitempty"`
	CacheHit         bool               `json:"cacheHit"`
	SwappedUseIDs    []string           `json:"swappedUseIds"`
	RequestBodyBytes int64              `json:"requestBodyBytes"`
	ResponseBytes    int64              `json:"responseBytes"`
	Upgrade          bool               `json:"upgrade"`
	UpgradeType      string             `json:"upgradeType,omitempty"`
}

// HTTPAuditExchangeDetail is one audited exchange in full: every field the
// pool's proxy recorded about it, rather than the summary a list carries
// (ADR 0130 §5). The bodies and any upgraded stream stay on the pool and are
// read with OpenHTTPAuditArtifact; the Recorded flags say which of them exist.
//
// Headers are redacted by the recorder as it writes them, so a credential the
// proxy swapped into a request was never in the row this returns.
type HTTPAuditExchangeDetail struct {
	HTTPAuditExchange
	EnqueuedAt           time.Time           `json:"enqueuedAt"`
	WrittenAt            time.Time           `json:"writtenAt"`
	RequestHeaders       map[string][]string `json:"requestHeaders"`
	ResponseHeaders      map[string][]string `json:"responseHeaders"`
	AppliedRuleID        string              `json:"appliedRuleId,omitempty"`
	AppliedPattern       string              `json:"appliedPattern,omitempty"`
	AppliedHeaders       []string            `json:"appliedHeaders"`
	CacheKey             string              `json:"cacheKey,omitempty"`
	CacheStored          bool                `json:"cacheStored"`
	CacheError           string              `json:"cacheError,omitempty"`
	RequestBodyFormat    string              `json:"requestBodyFormat,omitempty"`
	RequestBodyError     string              `json:"requestBodyError,omitempty"`
	RequestBodyRecorded  bool                `json:"requestBodyRecorded"`
	ResponseBodyFormat   string              `json:"responseBodyFormat,omitempty"`
	ResponseBodyError    string              `json:"responseBodyError,omitempty"`
	ResponseBodyRecorded bool                `json:"responseBodyRecorded"`
	UpgradeC2SBytes      int64               `json:"upgradeC2sBytes"`
	UpgradeS2CBytes      int64               `json:"upgradeS2cBytes"`
	StreamSessionID      string              `json:"streamSessionId,omitempty"`
	StreamFormat         string              `json:"streamFormat,omitempty"`
	StreamRecorded       bool                `json:"streamRecorded"`
	StreamDroppedChunks  int64               `json:"streamDroppedChunks"`
	StreamDroppedBytes   int64               `json:"streamDroppedBytes"`
}

// PoolRuntime is the provider surface for a pool's own runtime host: the pool
// is its own runtime host, so these converge and operate one container/VM/pod.
// The caller owns pool lifecycle persistence and job semantics. RepairPool is
// only for preserving in-place repair of pools with assigned sandboxes;
// delete reconciliation must use RemovePool and must not fall back to repair.
// begin records startup before a runtime is replaced or created; providers
// must call it before preloading images or launching a new agent.
type PoolRuntime interface {
	ReconcilePool(ctx context.Context, manager PoolManager, project *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool, images []string, begin func(context.Context) error) error
	RepairPool(ctx context.Context, manager PoolManager, project *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool, reason string, images []string, begin func(context.Context) error) error
	RemovePool(ctx context.Context, manager PoolManager, project *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool) error
	// ClearCache stops every running sandbox on the pool and empties the pool's
	// caches, returning the sandboxes it stopped. The pool agent does the work
	// and this answers once it is done; nothing is started again afterwards.
	// An agent too old to have the operation is ErrPoolAgentUnsupported.
	ClearCache(ctx context.Context, pool *model.Pool) ([]string, error)
	// ListHTTPAudit reads the HTTP exchanges the pool's proxy audited, newest
	// first, through the pool agent (ADR 0130 §4). A query naming a sandbox is
	// narrowed to it by the pool agent's token, not only by the filter. An agent
	// too old to have the operation is ErrPoolAgentUnsupported.
	ListHTTPAudit(ctx context.Context, pool *model.Pool, query HTTPAuditQuery) ([]HTTPAuditExchange, error)
	// GetHTTPAudit reads one audited exchange in full — every field the pool's
	// proxy recorded — narrowed to sandboxID when it is set. A record outside
	// that scope is ErrNotFound.
	GetHTTPAudit(ctx context.Context, pool *model.Pool, sandboxID string, id auditid.ExchangeID) (*HTTPAuditExchangeDetail, error)
	// OpenHTTPAuditArtifact streams one artifact recorded beside audit row id on
	// the pool — "request-body", "response-body" or "stream" — narrowed to
	// sandboxID when it is set. A row outside that scope, or with no such
	// artifact, is ErrNotFound.
	OpenHTTPAuditArtifact(ctx context.Context, pool *model.Pool, sandboxID string, id auditid.ExchangeID, artifact string) (*HTTPAuditArtifact, error)
	// ListDNSAudit reads the DNS queries the pool answered for its sandboxes,
	// newest first, through the pool agent, narrowed as ListHTTPAudit is. An
	// agent too old to have the operation is ErrPoolAgentUnsupported.
	ListDNSAudit(ctx context.Context, pool *model.Pool, filter DNSAuditFilter) ([]DNSAuditQuery, error)
	// OpenConsole attaches to the pool host's administrative console: a root
	// shell in the host's own namespaces, for operators debugging the backend
	// itself. It deliberately does not go through the pool agent, because the
	// agent is one of the things a broken host stops running.
	OpenConsole(ctx context.Context, provider *model.SandboxProviderInstance, pool *model.Pool, opts ConsoleOptions) (PTY, error)
	// OpenLogs reads what the backend itself recorded about the pool's host: a
	// VM's serial console, the Docker daemon's journal. Like OpenConsole it
	// goes through the driver rather than the pool agent, for the same reason —
	// the host whose log is worth reading is usually the host whose agent never
	// came up — and it is the one of the two that answers on a host with no
	// shell to attach to at all.
	OpenLogs(ctx context.Context, provider *model.SandboxProviderInstance, pool *model.Pool, opts PoolLogOptions) (*PoolLogStream, error)
	// BuildGuestImage rebuilds the guest image this backend boots, on the pool
	// host that is already running it, and writes the artifacts where the
	// backend looks for a local build (ADR 0062 §7).
	//
	// It closes the bootstrap loop: a Mac has no Docker daemon to build a guest
	// on, and the only one it can reach is the one inside a pool VM — which is
	// booted from the guest image being replaced. So the running guest builds
	// its own successor, and the pool is recreated to adopt it.
	//
	// A backend that boots no guest image returns ErrGuestImageBuildUnsupported.
	BuildGuestImage(ctx context.Context, provider *model.SandboxProviderInstance, pool *model.Pool, opts GuestImageBuildOptions) (*GuestImageBuild, error)
}

// GuestImageBuildOptions configures one rebuild of a backend's guest image.
type GuestImageBuildOptions struct {
	// SourceDir is the checkout the guest image is built from, on the machine
	// running the control plane. It is the build context and the directory the
	// backend's Dockerfile path is resolved against.
	//
	// The caller names it because the server has no other way to know where a
	// developer's checkout is, and it is read with the control plane's own
	// privileges. That is the same trust an administrative pool console already
	// carries — a root shell on the pool host — and this operation is gated the
	// same way.
	SourceDir string
	// RestartHost stops the pool's host once the artifacts are published, so the
	// pool's own reconcile brings it back on the guest that was just built.
	//
	// It is not the default and it is not free: a VM boots the artifacts it was
	// started with, so nothing adopts a new guest until the machine is replaced,
	// and replacing it stops every sandbox running on that pool. The caller
	// decides which of those two costs they are paying.
	RestartHost bool
}

// GuestImageBuild is one running guest image build: its output as it happens,
// and where the artifacts land.
//
// It streams because the build is minutes of work on a machine the caller
// cannot see, and because the caller asking for it is a developer watching it.
// The build runs while the stream is read and stops when it is closed.
//
// A failure is reported in the stream rather than by the call: the response has
// begun by the time anything can go wrong. The transport carries the outcome
// out of band — see the guest build error trailer the HTTP route sets — so a
// client learns of a failure without parsing the build's own output.
type GuestImageBuild struct {
	// Destination is the directory the artifacts are written to on success.
	Destination string
	io.ReadCloser
}

// PoolLogOptions configures one read of a pool host's backend log.
type PoolLogOptions struct {
	// Tail bounds the read to the last N lines. Zero takes whatever the
	// backend gives by default, which for a whole VM console is the whole log.
	Tail int
	// Follow keeps the stream open, appending as the host writes, until the
	// caller closes it or its context ends.
	Follow bool
}

// PoolLogStream is one open read of a pool host's backend log.
//
// Source names what was opened — "guest serial console", "docker daemon
// journal (systemd)" — because there is no uniform pool host log: each backend
// keeps a different record in a different place, and an operator reading the
// bytes has to know which record they got. It is descriptive text for a human,
// not an identifier to switch on.
type PoolLogStream struct {
	Source string
	io.ReadCloser
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
