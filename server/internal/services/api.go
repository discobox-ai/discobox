// Package services defines server service contracts using generated OpenAPI types.
package services

import (
	"context"
	"io"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/auditid"
	"github.com/discobox-ai/discobox/sandboxmeta"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

type ApproveSecretRequestBody = apimodel.ApproveSecretRequestBody
type CreateHarnessConfigBody = apimodel.CreateHarnessConfigBody
type SetHarnessConfigSecretBindingBody = apimodel.SetHarnessConfigSecretBindingBody
type CreateSecretBody = apimodel.CreateSecretBody
type CreateSecretRequestBody = apimodel.CreateSecretRequestBody
type CreateSandboxCredentialRequestBody = apimodel.CreateSandboxCredentialRequestBody
type RecordCredentialVerdictBody = apimodel.RecordCredentialVerdictBody
type CreateSecretGrantBody = apimodel.CreateSecretGrantBody
type CreateSSHKeyBody = apimodel.CreateSSHKeyBody
type CreatePeerBody = apimodel.CreatePeerBody
type UpdateHarnessConfigBody = apimodel.UpdateHarnessConfigBody
type UpdateSecretBody = apimodel.UpdateSecretBody
type CreateSandboxBody = apimodel.CreateSandboxBody
type CompleteSandboxSourcePushBody = apimodel.CompleteSandboxSourcePushBody
type CompleteSandboxApplyBody = apimodel.CompleteSandboxApplyBody
type UpdateSandboxMetaBody = apimodel.UpdateSandboxMetaBody
type SandboxSecretInput = apimodel.SandboxSecretInput
type UpdateSandboxBody = apimodel.UpdateSandboxBody
type StartSandboxBody = apimodel.StartSandboxBody
type StopSandboxBody = apimodel.StopSandboxBody
type RestartSandboxBody = apimodel.RestartSandboxBody
type UpgradeSandboxBody = apimodel.UpgradeSandboxBody
type SandboxProviderCatalogItem = apimodel.SandboxProviderCatalogItem
type ProviderConfigField = apimodel.ProviderConfigField
type ProviderStatus = apimodel.ProviderStatus
type CreateSandboxProviderInstanceBody = apimodel.CreateSandboxProviderInstanceBody
type UpdateSandboxProviderInstanceBody = apimodel.UpdateSandboxProviderInstanceBody
type CreateProjectBody = apimodel.CreateProjectBody
type UpdateProjectBody = apimodel.UpdateProjectBody
type CreatePoolBody = apimodel.CreatePoolBody
type UpdatePoolBody = apimodel.UpdatePoolBody
type RegisterPoolBody = apimodel.RegisterPoolBody
type RegisterPoolResponseBody = apimodel.RegisterPoolResponseBody
type UpdatePoolStatusBody = apimodel.UpdatePoolStatusBody
type ReportPoolSandboxStatesBody = apimodel.ReportPoolSandboxStatesBody
type MintSandboxAgentStatusTokensBody = apimodel.MintSandboxAgentStatusTokensBody
type MintSandboxAgentStatusTokensResponseBody = apimodel.MintSandboxAgentStatusTokensResponseBody
type SandboxAgentStatusToken = apimodel.SandboxAgentStatusToken
type ReportPoolResourcesBody = apimodel.ReportPoolResourcesBody
type ReportSandboxAgentStatusBody = apimodel.ReportSandboxAgentStatusBody
type OptBool = serverapi.OptBool
type OptString = serverapi.OptString
type OptURI = serverapi.OptURI
type OptInt64 = serverapi.OptInt64
type OptSandboxUser = serverapi.OptSandboxUser
type OptSandboxGitIdentity = serverapi.OptSandboxGitIdentity
type OptNilProviderConfigFieldArray = serverapi.OptNilProviderConfigFieldArray
type OptSandboxCreateConfigSourceCodeReferences = serverapi.OptSandboxCreateConfigSourceCodeReferences
type HTTPClientLease = transport.HTTPClientLease

// ProjectService manages projects and the user's default project.
type ProjectService interface {
	ListProjects(ctx context.Context) ([]model.Project, error)
	CreateProject(ctx context.Context, input CreateProjectBody) (*model.Project, error)
	GetProject(ctx context.Context, projectID string) (*model.Project, error)
	UpdateProject(ctx context.Context, projectID string, input UpdateProjectBody) (*model.Project, error)
	DeleteProject(ctx context.Context, projectID string) error
	// SetDefaultProject moves the calling user's default-project flag, which is
	// what the "default" project alias resolves. It has no unset: a user always
	// has exactly one default project.
	SetDefaultProject(ctx context.Context, projectID string) (*model.Project, error)
}

// HarnessConfigService manages project-scoped harness configurations.
type HarnessConfigService interface {
	ListHarnessConfigs(ctx context.Context, projectID string) ([]model.HarnessConfig, error)
	CreateHarnessConfig(ctx context.Context, projectID string, input CreateHarnessConfigBody) (*model.HarnessConfig, error)
	GetHarnessConfig(ctx context.Context, projectID, configID string) (*model.HarnessConfig, error)
	UpdateHarnessConfig(ctx context.Context, projectID, configID string, input UpdateHarnessConfigBody) (*model.HarnessConfig, error)
	SetDefaultHarnessConfig(ctx context.Context, projectID, configID string) (*model.Project, error)
	UnsetDefaultHarnessConfig(ctx context.Context, projectID, configID string) (*model.Project, error)
	DeleteHarnessConfig(ctx context.Context, projectID, configID string) error

	// ConfigureHarnessConfig launches the harness's interactive configure sandbox
	// and returns it. The caller seeds it via AttachHarnessConfigConfigure, attaches
	// to its primary terminal, then calls CommitHarnessConfigConfigure. Re-running
	// is allowed and clobbers any in-flight attempt.
	ConfigureHarnessConfig(ctx context.Context, projectID, configID string) (*model.Sandbox, error)
	// AttachHarnessConfigConfigure seeds the previous configuration into the
	// in-flight configure sandbox. Call it before attaching to the primary terminal.
	AttachHarnessConfigConfigure(ctx context.Context, projectID, configID string) error
	// CommitHarnessConfigConfigure verifies the configure command exited 0, applies
	// what it wrote, and deletes the configure sandbox.
	CommitHarnessConfigConfigure(ctx context.Context, projectID, configID string) (*model.HarnessConfig, error)
	// DeconfigureHarnessConfig removes the assets the configure flow created and
	// marks the config unconfigured.
	DeconfigureHarnessConfig(ctx context.Context, projectID, configID string) (*model.HarnessConfig, error)
	// RefreshHarnessConfigImage re-inspects the config's image and re-snapshots
	// its label metadata and digest.
	RefreshHarnessConfigImage(ctx context.Context, projectID, configID string) (*model.HarnessConfig, error)
	// EnsureHarnessAvailable fails when a project has no harness config at all,
	// naming what was wrong with each built-in image. Not a route: it is the
	// invariant server startup checks before it hands over, since a project
	// with no harness has no sandbox it can create (ADR 0048).
	EnsureHarnessAvailable(ctx context.Context, projectID string) error

	ListHarnessConfigSecretBindings(ctx context.Context, projectID, configID string) ([]model.HarnessConfigSecretBinding, error)
	SetHarnessConfigSecretBinding(ctx context.Context, projectID, configID, envName, secretID string) (*model.HarnessConfigSecretBinding, error)
	DeleteHarnessConfigSecretBinding(ctx context.Context, projectID, configID, envName string) error
}

// AcquireSandboxHTTPClient takes no set of acceptable sandbox states, so no
// caller has to know the sandbox is up before it can talk to it.
//
// Under ADR 0017 §12 the pool agent starts a stopped sandbox on demand, so the
// server checks only that the sandbox exists and is not being deleted, and
// that its pool is reachable. Gating on power state would refuse traffic that
// the pool agent would happily have served, and it would only cover the routes
// that consult the server in the first place.

// SandboxService manages sandboxes within a project.
type SandboxService interface {
	// FallbackHarnessConfig is the project's reserved `shell` config, which a
	// legacy sandbox with no harness config of its own upgrades to
	// (ADR 0032 §4); create never ends at it (ADR 0048). The API mappers need
	// it to report that upgrade. Nil when seeding has not created it.
	FallbackHarnessConfig(ctx context.Context, projectID string) (*model.HarnessConfig, error)
	// ListSandboxes filters on the sandboxes' recorded tags as well as where
	// they came from; no selectors lists them whatever their tags.
	ListSandboxes(ctx context.Context, projectID, sourceRoot string, originKeys []string, tags []sandboxmeta.Selector) ([]model.Sandbox, error)
	CreateSandbox(ctx context.Context, projectID string, input CreateSandboxBody) (*model.Sandbox, error)
	GetSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error)
	UpdateSandbox(ctx context.Context, projectID, sandboxID string, input UpdateSandboxBody) (*model.Sandbox, error)
	// DeleteSandbox archives; UnarchiveSandbox restores; PurgeSandbox destroys
	// the data and confirms it before returning (ADR 0022 §§2-3).
	DeleteSandbox(ctx context.Context, projectID, sandboxID string) error
	UnarchiveSandbox(ctx context.Context, projectID, sandboxID string) error
	PurgeSandbox(ctx context.Context, projectID, sandboxID string) error
	StartSandbox(ctx context.Context, projectID, sandboxID string, input StartSandboxBody) (*model.Sandbox, error)
	StopSandbox(ctx context.Context, projectID, sandboxID string, input StopSandboxBody) (*model.Sandbox, error)
	RestartSandbox(ctx context.Context, projectID, sandboxID string, input RestartSandboxBody) (*model.Sandbox, error)
	// RepairSandbox rebuilds the sandbox in place — teardown, recreate against
	// the durable tree, then a start instruction — as one intent (ADR 0035).
	// The rebuild lands on the harness config's current image: the same intent
	// carries the re-pin an upgrade would (ADR 0062).
	RepairSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error)
	// UpgradeSandbox re-pins the sandbox to its harness config's current image.
	// The pool agent recreates the container from it and keeps the power state
	// it had (ADR 0021).
	UpgradeSandbox(ctx context.Context, projectID, sandboxID string, input UpgradeSandboxBody) (*model.Sandbox, error)
	CompleteSandboxSourcePush(ctx context.Context, projectID, sandboxID string, input CompleteSandboxSourcePushBody) (*model.Sandbox, error)
	CompleteSandboxApply(ctx context.Context, projectID, sandboxID string, input CompleteSandboxApplyBody) (*model.Sandbox, error)
	// UpdateSandboxMeta carries a change to the description or tags into the
	// sandbox, which holds them, and records what it holds afterwards
	// (ADR 0136).
	UpdateSandboxMeta(ctx context.Context, projectID, sandboxID string, input UpdateSandboxMetaBody) (*model.Sandbox, error)
	ReconcileSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error)
	AcquireSandboxHTTPClient(ctx context.Context, projectID, sandboxID string, scopes []string) (*HTTPClientLease, *model.Sandbox, error)
	// AwaitSandboxHTTPClient is the same acquire for a caller that means "I
	// want to use this sandbox now": it waits for a sandbox that is still
	// being provisioned to become reachable instead of refusing the request
	// (ADR 0039 tier 1). Callers that want the fail-fast answer keep using
	// AcquireSandboxHTTPClient.
	AwaitSandboxHTTPClient(ctx context.Context, projectID, sandboxID string, scopes []string) (*HTTPClientLease, *model.Sandbox, error)
	AssignSandboxHarnessSecrets(ctx context.Context, projectID, sandboxID, harnessConfigID string) (map[string]string, error)
	// ExportSandbox streams the discobox as a `.dbox` archive: its spec, then
	// its durable tree (ADR 0123 §1). It refuses a running discobox, because a
	// tar of a live tree is not a consistent one. The stream is the caller's to
	// close.
	ExportSandbox(ctx context.Context, projectID, sandboxID string) (io.ReadCloser, error)
	// ImportSandbox creates a discobox from such an archive, restoring the tree
	// onto its pool before the row that wakes the reconciler exists
	// (ADR 0123 §3).
	ImportSandbox(ctx context.Context, projectID string, archive io.Reader, opts SandboxImportOptions) (*SandboxImportResult, error)
}

// SandboxImportOptions are the caller's overrides for one import. Everything
// else comes from the archive.
type SandboxImportOptions struct {
	// Name renames the imported discobox. Empty keeps the exported name, which
	// is refused when the project already has one.
	Name string
	// PoolID places it. Empty takes the project's default pool, as a create
	// does.
	PoolID string
	// HarnessSlug overrides the harness the archive names, for a destination
	// that calls the same harness something else.
	HarnessSlug string
}

// SandboxImportResult is one completed import: the discobox, and what could not
// be carried over with it.
type SandboxImportResult struct {
	Sandbox *model.Sandbox
	// Warnings are things the caller should know about a discobox that was
	// created nonetheless -- a secret binding with no secret of that name here,
	// above all. Refusing the whole import over a credential the user can add
	// afterwards would throw away a transferred workspace to save them one
	// command.
	Warnings []string
}

// HTTPAuditFilter narrows ListHTTPAudit. A zero field matches everything.
type HTTPAuditFilter struct {
	// SandboxID is matched against the recorded client ID and never requires
	// the sandbox to exist: its exchanges outlive it on its pool.
	SandboxID string
	PoolID    string
	Host      string
	UseID     string
	Since     time.Time
	// MinStatus and MaxStatus bound the response status, inclusive; zero
	// leaves that side open.
	MinStatus int
	MaxStatus int
	// Blocked selects refused exchanges when true and admitted ones when
	// false; nil is both.
	Blocked *bool
	// Ascending reads oldest first from Since, which is how a follower reads
	// forward without missing a row between polls.
	Ascending bool
	// After is a per-pool cursor, keyed by pool ID: that pool is read in write
	// order after the record given, instead of by Since. A pool with no entry
	// is read by Since, which is how a pool whose records the caller has not
	// seen yet joins a follow already running.
	After map[string]auditid.ExchangeID
	Limit int
}

// HTTPAuditResult is a merged read of every pool that was asked.
type HTTPAuditResult struct {
	Exchanges []PoolHTTPAuditExchange `json:"exchanges"`
	// UnavailablePools are the pools asked that did not answer. A non-empty
	// list means exchanges may be missing, and the caller must be told.
	UnavailablePools []UnavailableAuditPool `json:"unavailablePools"`
}

// PoolHTTPAuditExchange is an audited exchange and the pool that recorded it.
// Row IDs are only unique within a pool.
type PoolHTTPAuditExchange struct {
	PoolID string `json:"poolId"`
	sandbox.HTTPAuditExchange
}

// PoolHTTPAuditExchangeDetail is one audited exchange in full and the pool that
// recorded it. As with PoolHTTPAuditExchange the pool is carried beside the
// record rather than in it: an audit row does not know which pool holds it, and
// its ID means nothing without one.
type PoolHTTPAuditExchangeDetail struct {
	PoolID string `json:"poolId"`
	sandbox.HTTPAuditExchangeDetail
}

// UnavailableAuditPool is a pool whose part of the trail could not be read.
type UnavailableAuditPool struct {
	PoolID string `json:"poolId"`
	Reason string `json:"reason"`
}

type SandboxProviderInstanceService interface {
	ListSandboxProviderCatalogItems(ctx context.Context) ([]SandboxProviderCatalogItem, error)
	ListSandboxProviderInstances(ctx context.Context, projectID string) ([]model.SandboxProviderInstance, error)
	CreateSandboxProviderInstance(ctx context.Context, projectID string, input CreateSandboxProviderInstanceBody) (*model.SandboxProviderInstance, error)
	GetSandboxProviderInstance(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error)
	UpdateSandboxProviderInstance(ctx context.Context, projectID, providerID string, input UpdateSandboxProviderInstanceBody) (*model.SandboxProviderInstance, error)
	DeleteSandboxProviderInstance(ctx context.Context, projectID, providerID string) error
}

// PoolService manages project-scoped pools: the user-visible sharing boundary
// sandboxes are scheduled into, each its own runtime host. It also carries the
// pool agent surface: registration, heartbeats, and sandbox-state reports.
type PoolService interface {
	ListPools(ctx context.Context, projectID string) ([]model.Pool, error)
	CreatePool(ctx context.Context, projectID string, input CreatePoolBody) (*model.Pool, error)
	GetPool(ctx context.Context, projectID, poolID string) (*model.Pool, error)
	UpdatePool(ctx context.Context, projectID, poolID string, input UpdatePoolBody) (*model.Pool, error)
	DeletePool(ctx context.Context, projectID, poolID string) error
	SetDefaultPool(ctx context.Context, projectID, poolID string) (*model.Project, error)
	UnsetDefaultPool(ctx context.Context, projectID, poolID string) (*model.Project, error)
	ReconcilePool(ctx context.Context, projectID, poolID string) (*model.Pool, error)
	// ClearPoolCache has the pool agent stop every running sandbox on the pool
	// and empty the pool's caches, and returns the sandboxes it stopped once the
	// cache is empty.
	ClearPoolCache(ctx context.Context, projectID, poolID string) ([]string, error)
	// ListHTTPAudit reads the HTTP exchanges the project's pool proxies
	// audited, from every pool the filter allows, merged newest first (ADR 0130
	// §§1, 4). A pool that does not answer is reported, not dropped.
	ListHTTPAudit(ctx context.Context, projectID string, filter HTTPAuditFilter) (*HTTPAuditResult, error)
	// GetHTTPAudit reads one audited exchange in full from the pool that
	// recorded it, narrowed to sandboxID when it is set.
	GetHTTPAudit(ctx context.Context, projectID, poolID, sandboxID string, id auditid.ExchangeID) (*PoolHTTPAuditExchangeDetail, error)
	// OpenHTTPAuditArtifact streams a body or upgraded stream recorded beside
	// one audited exchange on one pool, narrowed to sandboxID when it is set.
	// The caller closes the artifact's body.
	OpenHTTPAuditArtifact(ctx context.Context, projectID, poolID, sandboxID string, id auditid.ExchangeID, artifact string) (*sandbox.HTTPAuditArtifact, error)
	// OpenPoolConsole attaches to the pool host's administrative console: a
	// privileged root shell on the machine running the pool's runtime, for
	// debugging the backend itself.
	OpenPoolConsole(ctx context.Context, projectID, poolID string, opts sandbox.ConsoleOptions) (sandbox.PTY, error)
	// OpenPoolLogs reads whatever the pool's backend recorded about the machine
	// running its runtime: a VM's serial console, a Docker daemon's journal.
	OpenPoolLogs(ctx context.Context, projectID, poolID string, opts sandbox.PoolLogOptions) (*sandbox.PoolLogStream, error)
	// BuildPoolGuestImage rebuilds the guest image the pool's backend boots, on
	// that pool's own host, and publishes the artifacts where the backend looks
	// for a local build (ADR 0062 §7).
	BuildPoolGuestImage(ctx context.Context, projectID, poolID string, opts sandbox.GuestImageBuildOptions) (*sandbox.GuestImageBuild, error)

	RegisterPool(ctx context.Context, input RegisterPoolBody) (*RegisterPoolResponseBody, error)
	UpdatePoolStatus(ctx context.Context, poolID string, input UpdatePoolStatusBody) (*model.Pool, error)
	ReportPoolSandboxStates(ctx context.Context, poolID string, input ReportPoolSandboxStatesBody) error
	MintSandboxAgentStatusTokens(ctx context.Context, poolID string, input MintSandboxAgentStatusTokensBody) (*MintSandboxAgentStatusTokensResponseBody, error)
	ReportSandboxAgentStatus(ctx context.Context, poolID string, input ReportSandboxAgentStatusBody) error
	ReportPoolResources(ctx context.Context, poolID string, input ReportPoolResourcesBody) error
}

// JobService exposes a project's pending reconcile work as jobs: each is a
// dirty mark in the reconcile engine, not a stored row.
type JobService interface {
	GetJob(ctx context.Context, projectID, jobID string) (*model.Job, error)
	ForceJob(ctx context.Context, projectID, jobID string) (*model.Job, error)
	ListJobs(ctx context.Context, projectID string) ([]model.Job, error)
}

// SecretService manages project-scoped secrets and their request/approval lifecycle.
type SecretService interface {
	ListSecrets(ctx context.Context, projectID string) ([]model.Secret, error)
	CreateSecret(ctx context.Context, projectID string, input CreateSecretBody) (*model.Secret, error)
	GetSecret(ctx context.Context, projectID, secretID string) (*model.Secret, error)
	UpdateSecret(ctx context.Context, projectID, secretID string, input UpdateSecretBody) (*model.Secret, error)
	DeleteSecret(ctx context.Context, projectID, secretID string) error

	ListSecretRequests(ctx context.Context, projectID, status string) ([]model.SecretRequest, error)
	CreateSecretRequest(ctx context.Context, projectID string, input CreateSecretRequestBody) (*model.SecretRequest, error)
	GetSecretRequest(ctx context.Context, projectID, requestID string) (*model.SecretRequest, error)
	ApproveSecretRequest(ctx context.Context, projectID, requestID string, input ApproveSecretRequestBody) (*model.SecretRequest, error)
	DenySecretRequest(ctx context.Context, projectID, requestID string) error

	ListSecretGrants(ctx context.Context, projectID, secretID string) ([]model.SecretGrant, error)
	CreateSecretGrant(ctx context.Context, projectID string, input CreateSecretGrantBody) (*model.SecretGrant, error)
	RevokeSecretGrant(ctx context.Context, projectID, grantID string) error

	ResolveSandboxSecret(ctx context.Context, poolID, sandboxID, sentinel, host string) (*model.SandboxSecretResolution, error)
	// RecordSandboxSecretRejection takes what an upstream made of a credential
	// the pool's proxy swapped in, and decides whether it needs a person
	// (ADR 0132). ListSecretRejections is what the window reads back.
	RecordSandboxSecretRejection(ctx context.Context, poolID, sandboxID, sentinel, host, outcome, useID string) error
	ListSecretRejections(ctx context.Context, projectID string) ([]model.SecretRejection, error)

	// The agent credentials broker (ADR 0031). Every call is made by a pool
	// agent on behalf of one of its own sandboxes, so each takes the calling
	// pool's ID and verifies the sandbox belongs to it.
	ListSandboxCredentials(ctx context.Context, poolID, sandboxID string) ([]store.AgentCredential, error)
	CreateSandboxCredentialRequest(ctx context.Context, poolID string, input CreateSandboxCredentialRequestBody) (*model.SecretRequest, error)
	GetSandboxCredentialRequest(ctx context.Context, poolID, sandboxID, requestID string) (*model.SecretRequest, *model.SecretGrant, error)
	// RecordCredentialVerdict persists one judge decision, so a credential
	// cannot be issued without a record of why (ADR 0091).
	RecordCredentialVerdict(ctx context.Context, poolID string, input RecordCredentialVerdictBody) error
	// ListCredentialVerdicts reads that record back for a project's members.
	// Unlike the broker calls above it is a user read, scoped by project, and
	// does not require the sandbox a verdict names to still exist.
	ListCredentialVerdicts(ctx context.Context, projectID string, filter CredentialVerdictFilter) ([]model.CredentialVerdict, error)
}

// CredentialVerdictFilter is the store's filter, named here so a handler builds
// it through its services dependency rather than importing internal/store.
type CredentialVerdictFilter = store.CredentialVerdictFilter

// SSHKeyService manages project-scoped SSH keys that authorize SSH access to
// that project's sandboxes (ADR 0024 §5).
type SSHKeyService interface {
	ListSSHKeys(ctx context.Context, projectID string) ([]model.SSHKey, error)
	CreateSSHKey(ctx context.Context, projectID string, input CreateSSHKeyBody) (*model.SSHKey, error)
	DeleteSSHKey(ctx context.Context, projectID, keyID string) error
}

// PeerService manages the iroh endpoint IDs enrolled on this server (ADR
// 0095). It is server-scoped rather than project-scoped: an iroh connection
// carries the entire control-plane API, so an enrolled ID authenticates as a
// user and the existing authorization pipeline decides the rest.
//
// It is the managed half of two layers. The other is the authorized_ids file,
// which is deliberately not reachable from here — it is what an operator falls
// back to when the API is what they are trying to reach (ADR 0095 §3, enrolled iroh IDs).
type PeerService interface {
	ListPeers(ctx context.Context) ([]model.Peer, error)
	CreatePeer(ctx context.Context, input CreatePeerBody) (*model.Peer, error)
	DeletePeer(ctx context.Context, idOrPrefix string) error
}

// SSHIngress is what a client needs to verify this server's SSH ingress (ADR
// 0024, ADR 0057). It is a resolved value rather than a service: the host key
// is loaded once at startup, so there is nothing to call.
//
// The host key is all of it. Every server serves SSH, over the one transport
// its API already answers on (`GET /ssh/connect`), so there is no address to
// advertise and nothing to enable — only a key to pin.
type SSHIngress struct {
	HostKey string
}

// ServerPeer is this server's own peer identity, served by GET /peer so a
// client that already reaches this server some other way can learn the address
// to dial it at (ADR 0098). Like SSHIngress it is a resolved value rather than
// a service: the identity is loaded once, at the point the iroh endpoint is
// configured, and cannot change while the process runs — it is the address.
//
// Every server has one, whatever it listens on: the key is loaded on every
// start, and only the iroh endpoint is opt-in (ADR 0117).
type ServerPeer struct {
	ID string
}

// ServerInfo is what GET /server serves: what this server calls itself, which
// a client offers as the name to register it under (ADR 0116 §2). It is
// configuration resolved at startup, fixed for the life of the process.
type ServerInfo struct {
	Name string
}

// IrohListener is what this server's iroh listener is doing right now.
//
// Unlike [ServerPeer], which is an identity resolved once and fixed for the
// life of the process, this changes underneath a running server — and it is the
// change nobody could see. A listener that loses its relay leaves the process
// up, the unix socket answering and /healthz saying ready, while every client
// dialing its peer ID times out. Reporting it here means an operator who can
// reach this server by any transport at all can ask about the one that is
// broken.
type IrohListener struct {
	// Online reports whether the listener has a relay right now. Without one it
	// is reachable only from networks that can route to its sockets directly.
	Online bool
	// HomeRelay is the relay it is reachable through, empty when it has none.
	HomeRelay string
	// Since is when Online last changed, so a report can say how long a
	// listener has been off its relay rather than only that it is.
	Since time.Time
	// Sockets are the local UDP addresses it is bound to, and DirectAddrs the
	// addresses it believes peers can reach it at.
	Sockets     []string
	DirectAddrs []string
}

// IrohListenerService reports the listener's current state, and whether it has
// been read yet. A server with no iroh endpoint has a nil service: it has no
// listener rather than one that is down.
type IrohListenerService func() (IrohListener, bool)

// Services groups the dependencies needed by the API operations.
type Services struct {
	// SSH is served by GET /ssh so a client can pin the host key before it
	// holds any other credential.
	SSH SSHIngress

	// ServerPeer is served by GET /peer: who this server is, for a client
	// deciding what to dial. Peers, below, is the other direction — who this
	// server admits.
	ServerPeer ServerPeer

	// ServerInfo is served by GET /server: what this server calls itself.
	ServerInfo ServerInfo

	// IrohListener is served beside it: not who this server is, but whether the
	// transport clients dial is working. Nil when this server has no iroh
	// endpoint at all.
	IrohListener IrohListenerService

	Projects       ProjectService
	HarnessConfigs HarnessConfigService
	Sandboxes      SandboxService
	Providers      SandboxProviderInstanceService
	Pools          PoolService
	Jobs           JobService
	Secrets        SecretService
	SSHKeys        SSHKeyService
	Peers          PeerService
}
