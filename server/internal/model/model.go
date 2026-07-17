// Package model defines the combined database and API resource models.
//
// These structs intentionally carry GORM persistence tags together with JSON
// and OpenAPI-facing tags. If the API and database shapes diverge later,
// split only the affected resources into DTOs.
package model

import (
	"encoding/json"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/id"
	"github.com/obot-platform/discobox/internal/originkey"
)

const (
	WorkerSchedulingPreferred   = "preferred"
	WorkerSchedulingDegraded    = "degraded"
	WorkerSchedulingUnavailable = "unavailable"

	WorkerDesiredStateActive  = "active"
	WorkerDesiredStateDrained = "drained"
	WorkerDesiredStateDeleted = "deleted"

	WorkerPhasePending     = "pending"
	WorkerPhaseLaunching   = "launching"
	WorkerPhaseRegistering = "registering"
	WorkerPhaseActive      = "active"
	WorkerPhaseDraining    = "draining"
	WorkerPhaseDeleting    = "deleting"
	WorkerPhaseOffline     = "offline"
	WorkerPhaseFailed      = "failed"
	WorkerPhaseDeleted     = "deleted"

	WorkerOperationCreate = "create"
	WorkerOperationDrain  = "drain"
	WorkerOperationDelete = "delete"

	SandboxDesiredStateRunning = "running"
	SandboxDesiredStateStopped = "stopped"
	SandboxDesiredStateDeleted = "deleted"

	SandboxPhasePending      = "pending"
	SandboxPhaseProvisioning = "provisioning"
	// SandboxPhaseAwaitingSource means the sandbox is provisioned and waiting
	// for the client to push its source, because the source is a local
	// directory this server's provider cannot reach. The client pushes into the
	// sandbox's repository, then calls continue to name the commit to check out.
	SandboxPhaseAwaitingSource = "awaiting_source"
	SandboxPhaseStarting       = "starting"
	SandboxPhaseRunning        = "running"
	SandboxPhaseStopping       = "stopping"
	SandboxPhaseStopped        = "stopped"
	SandboxPhaseDeleting       = "deleting"
	SandboxPhaseDeleted        = "deleted"
	SandboxPhaseFailed         = "failed"

	// GitSourceDeliveryClone has the sandbox fetch the source itself.
	// GitSourceDeliveryPush has the client push it in, for a local directory the
	// provider cannot reach. Delivery is stated, never inferred from which
	// source fields happen to be set.
	GitSourceDeliveryClone = "clone"
	GitSourceDeliveryPush  = "push"

	SandboxOperationCreate  = "create"
	SandboxOperationStart   = "start"
	SandboxOperationStop    = "stop"
	SandboxOperationRestart = "restart"
	SandboxOperationDelete  = "delete"

	SandboxOperationStatusPending = OperationStatusPending
	SandboxOperationStatusRunning = OperationStatusRunning
	SandboxOperationStatusSuccess = OperationStatusSuccess
	SandboxOperationStatusFailed  = OperationStatusFailed
)

// Enum registries. Each slice is the canonical set of values a string-typed
// domain field may hold. They exist to be enumerated: the API layer serves
// these values verbatim (see services.Convert), and TestModelEnumsMatchAPISchema
// cross-checks every slice against the generated OpenAPI enum so the two lists
// cannot silently drift. Keep each slice in sync with the consts directly above
// it — a value present in one list but not the other is exactly the bug the test
// catches.
var (
	WorkerDesiredStates = []string{
		WorkerDesiredStateActive,
		WorkerDesiredStateDrained,
		WorkerDesiredStateDeleted,
	}
	WorkerPhases = []string{
		WorkerPhasePending,
		WorkerPhaseLaunching,
		WorkerPhaseRegistering,
		WorkerPhaseActive,
		WorkerPhaseDraining,
		WorkerPhaseDeleting,
		WorkerPhaseOffline,
		WorkerPhaseFailed,
		WorkerPhaseDeleted,
	}
	SandboxDesiredStates = []string{
		SandboxDesiredStateRunning,
		SandboxDesiredStateStopped,
		SandboxDesiredStateDeleted,
	}
	SandboxPhases = []string{
		SandboxPhasePending,
		SandboxPhaseProvisioning,
		SandboxPhaseAwaitingSource,
		SandboxPhaseStarting,
		SandboxPhaseRunning,
		SandboxPhaseStopping,
		SandboxPhaseStopped,
		SandboxPhaseDeleting,
		SandboxPhaseDeleted,
		SandboxPhaseFailed,
	}
	GitSourceDeliveries = []string{
		GitSourceDeliveryClone,
		GitSourceDeliveryPush,
	}
	OperationStatuses = []string{
		OperationStatusPending,
		OperationStatusRunning,
		OperationStatusSuccess,
		OperationStatusFailed,
	}
)

var (
	WorkerCreateOperation = OperationSpec{
		Operation:    WorkerOperationCreate,
		DesiredState: WorkerDesiredStateActive,
		Phase:        WorkerPhasePending,
	}
	WorkerDrainOperation = OperationSpec{
		Operation:    WorkerOperationDrain,
		DesiredState: WorkerDesiredStateDrained,
		Phase:        WorkerPhaseDraining,
	}
	WorkerDeleteOperation = OperationSpec{
		Operation:    WorkerOperationDelete,
		DesiredState: WorkerDesiredStateDeleted,
		Phase:        WorkerPhaseDeleting,
	}

	SandboxCreateOperation = OperationSpec{
		Operation:    SandboxOperationCreate,
		DesiredState: SandboxDesiredStateRunning,
		Phase:        SandboxPhasePending,
	}
	SandboxStartOperation = OperationSpec{
		Operation:    SandboxOperationStart,
		DesiredState: SandboxDesiredStateRunning,
		Phase:        SandboxPhaseStarting,
	}
	SandboxStopOperation = OperationSpec{
		Operation:    SandboxOperationStop,
		DesiredState: SandboxDesiredStateStopped,
		Phase:        SandboxPhaseStopping,
	}
	SandboxRestartOperation = OperationSpec{
		Operation:    SandboxOperationRestart,
		DesiredState: SandboxDesiredStateRunning,
		Phase:        SandboxPhaseStarting,
	}
	SandboxDeleteOperation = OperationSpec{
		Operation:    SandboxOperationDelete,
		DesiredState: SandboxDesiredStateDeleted,
		Phase:        SandboxPhaseDeleting,
	}
)

// User represents an authenticated user.
type User struct {
	ID        string         `gorm:"primaryKey;type:text" json:"id" doc:"Stable user ID"`
	Email     string         `gorm:"uniqueIndex;not null;type:text" json:"email" doc:"User email address" format:"email"`
	Name      *string        `gorm:"type:text" json:"name,omitempty" doc:"Display name"`
	AvatarURL *string        `gorm:"column:avatar_url;type:text" json:"avatarUrl,omitempty" doc:"Avatar image URL" format:"uri"`
	Provider  string         `gorm:"not null;type:text;uniqueIndex:idx_user_provider_subject" json:"provider" doc:"Authentication provider"`
	Subject   string         `gorm:"not null;type:text;uniqueIndex:idx_user_provider_subject" json:"subject" doc:"Provider subject identifier"`
	CreatedAt time.Time      `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time      `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

func (User) TableName() string { return "users" }

func (u *User) BeforeCreate(_ *gorm.DB) error {
	if u.ID == "" {
		var err error
		u.ID, err = id.New(id.PrefixUser)
		if err != nil {
			return err
		}
	}
	return nil
}

// Project groups sandboxes and provider configuration.
type Project struct {
	ID                       string         `gorm:"primaryKey;type:text" json:"id" doc:"Stable project ID"`
	OwnerUserID              string         `gorm:"column:owner_user_id;not null;type:text;index" json:"ownerUserId" doc:"Owning user ID"`
	Name                     string         `gorm:"not null;type:text" json:"name" doc:"Project display name" maxLength:"200"`
	Slug                     string         `gorm:"uniqueIndex;not null;type:text" json:"slug" doc:"URL-safe project slug" pattern:"^[a-z0-9][a-z0-9-]*$"`
	Default                  bool           `gorm:"column:default_project;not null;default:false;index" json:"default" doc:"Whether this is the user's default project"`
	DefaultSandboxProviderID string         `gorm:"column:default_sandbox_provider_id;type:text;default:''" json:"defaultSandboxProviderId,omitempty" doc:"Default sandbox provider instance ID"`
	DefaultHarnessConfigID   string         `gorm:"column:default_harness_config_id;type:text;default:''" json:"defaultHarnessConfigId,omitempty" doc:"Default harness config ID"`
	CreatedAt                time.Time      `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt                time.Time      `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`
	DeletedAt                gorm.DeletedAt `gorm:"index" json:"-"`

	Owner                    *User                     `gorm:"-" json:"owner,omitempty" doc:"Project owner"`
	Members                  []ProjectMember           `gorm:"foreignKey:ProjectID" json:"members,omitempty" doc:"Project members"`
	Sandboxes                []Sandbox                 `gorm:"foreignKey:ProjectID" json:"sandboxes,omitempty" doc:"Project sandboxes"`
	SandboxProviderInstances []SandboxProviderInstance `gorm:"foreignKey:ProjectID" json:"sandboxProviderInstances,omitempty" doc:"Sandbox provider instances"`
	HarnessConfigs           []HarnessConfig           `gorm:"foreignKey:ProjectID" json:"harnessConfigs,omitempty" doc:"Harness configurations"`
}

func (Project) TableName() string { return "projects" }

func (p *Project) BeforeCreate(_ *gorm.DB) error {
	if p.ID == "" {
		var err error
		p.ID, err = id.New(id.PrefixProject)
		if err != nil {
			return err
		}
	}
	return nil
}

// ProjectMember grants a user access to a project.
type ProjectMember struct {
	ProjectID string         `gorm:"column:project_id;primaryKey;type:text" json:"projectId" doc:"Project ID"`
	UserID    string         `gorm:"column:user_id;primaryKey;type:text" json:"userId" doc:"User ID"`
	Role      string         `gorm:"not null;type:text;default:'member'" json:"role" doc:"Project role"`
	CreatedAt time.Time      `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time      `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
	User    *User    `gorm:"-" json:"-"`
}

func (ProjectMember) TableName() string { return "project_members" }

// ServerState stores generic server settings and one-time state flags. Delete a
// row to allow its associated initialization to run again.
type ServerState struct {
	Key       string          `gorm:"primaryKey;type:text" json:"key" doc:"State key"`
	Value     json.RawMessage `gorm:"column:value;type:text" json:"value,omitempty" doc:"State value"`
	CreatedAt time.Time       `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time       `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`
}

func (ServerState) TableName() string { return "server_state" }

// SandboxAccessIssuerKey stores per-project, per-user key material used by the
// control plane to sign sandbox access tokens.
type SandboxAccessIssuerKey struct {
	ProjectID           string     `gorm:"column:project_id;primaryKey;type:text" json:"-"`
	UserID              string     `gorm:"column:user_id;primaryKey;type:text" json:"-"`
	PublicKey           string     `gorm:"column:public_key;not null;type:text" json:"-"`
	EncryptedPrivateKey []byte     `gorm:"column:encrypted_private_key;not null" json:"-"`
	KeyType             string     `gorm:"column:key_type;not null;type:text;default:'ed25519'" json:"-"`
	RotatedAt           *time.Time `gorm:"column:rotated_at" json:"-"`
	RevokedAt           *time.Time `gorm:"column:revoked_at;index" json:"-"`
	CreatedAt           time.Time  `gorm:"autoCreateTime" json:"-"`
	UpdatedAt           time.Time  `gorm:"autoUpdateTime" json:"-"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
	User    *User    `gorm:"-" json:"-"`
}

func (SandboxAccessIssuerKey) TableName() string { return "sandbox_access_issuer_keys" }

// ProjectUserKey is kept as a compatibility alias while callers migrate to the
// design-level SandboxAccessIssuerKey name.
type ProjectUserKey = SandboxAccessIssuerKey

// HarnessConfig stores a project-scoped harness runtime configuration.
//
// A harness config is the single harness concept: the three included harnesses
// are seeded as built-in configs rather than surfaced as separate definitions.
// A config is only selectable once Configured is true, which happens when its
// configure flow completes successfully; Deconfigure reverses that by removing
// the assets the flow created.
type HarnessConfig struct {
	ID              string                `gorm:"primaryKey;type:text" json:"id" doc:"Stable harness config ID"`
	ProjectID       string                `gorm:"column:project_id;not null;type:text;index;uniqueIndex:idx_harness_config_project_name,priority:1" json:"projectId" doc:"Project ID"`
	Slug            string                `gorm:"column:slug;not null;type:text;default:'';index" json:"slug" doc:"Stable, URL-safe identifier used to select the harness config (e.g. codex). Unique within the project." pattern:"^[a-z0-9][a-z0-9-]*$"`
	BuiltIn         bool                  `gorm:"column:built_in;not null;default:false" json:"builtIn" doc:"True for the included harnesses seeded by the server. Built-in configs track their image and cannot be deleted."`
	Configured      bool                  `gorm:"column:configured;not null;default:false" json:"configured" doc:"True once the configure flow completed successfully. Only configured harnesses can be selected to run."`
	Name            string                `gorm:"column:name;not null;type:text;uniqueIndex:idx_harness_config_project_name,priority:2" json:"name" doc:"Harness config name" maxLength:"200"`
	Image           string                `gorm:"column:image;not null;type:text;default:''" json:"image" doc:"Harness-specific sandbox image"`
	ImageDigest     string                `gorm:"column:image_digest;not null;type:text;default:''" json:"imageDigest,omitempty" doc:"Content digest observed when the harness image was registered"`
	RunCommand      []string              `gorm:"column:run_command;type:text;serializer:json" json:"runCommand,omitempty" doc:"Run argv snapshotted from the registered image label."`
	RelaunchCommand []string              `gorm:"column:relaunch_command;type:text;serializer:json" json:"relaunchCommand,omitempty" doc:"Relaunch argv snapshotted from the registered image label."`
	Files           []HarnessConfigFile   `gorm:"column:files;type:text;serializer:json" json:"files,omitempty" doc:"Non-secret files declared by the image label. Baseline only; the configure flow's files live in ConfiguredFiles."`
	Secrets         []HarnessConfigSecret `gorm:"column:secrets;type:text;serializer:json" json:"secrets,omitempty" doc:"Environment-variable secret declarations snapshotted from the image label."`
	// ConfiguredFiles and ConfiguredSecretIDs record what the configure flow
	// produced, kept separate from the image-declared baseline so Deconfigure can
	// remove exactly what it created and leave the baseline intact.
	ConfiguredFiles     []HarnessConfigFile `gorm:"column:configured_files;type:text;serializer:json" json:"configuredFiles,omitempty" doc:"Files written by the configure flow. Overlay the image-declared Files when resolving a sandbox."`
	ConfiguredSecretIDs []string            `gorm:"column:configured_secret_ids;type:text;serializer:json" json:"-" doc:"Secrets created by the configure flow, deleted on deconfigure."`
	// ConfigureSandboxID names the in-flight configure sandbox. It is the durable
	// handle the harness-config reconciler watches, so a configure that is running
	// when the server restarts is still picked up and finished.
	ConfigureSandboxID string    `gorm:"column:configure_sandbox_id;type:text;default:''" json:"configureSandboxId,omitempty" doc:"Sandbox running the configure flow, while one is in flight."`
	ConfigureError     string    `gorm:"column:configure_error;type:text;default:''" json:"configureError,omitempty" doc:"Why the last configure attempt failed. Cleared when a configure starts or succeeds."`
	CreatedAt          time.Time `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt          time.Time `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Project   *Project  `gorm:"foreignKey:ProjectID" json:"-"`
	Sandboxes []Sandbox `gorm:"foreignKey:HarnessConfigID" json:"-"`
}

func (HarnessConfig) TableName() string { return "harness_configs" }

func (a *HarnessConfig) EventProjectID() string { return a.ProjectID }

func (a *HarnessConfig) EventResourceType() string { return "harnessConfig" }

func (a *HarnessConfig) EventResourceID() string { return a.ID }

func (a *HarnessConfig) BeforeCreate(_ *gorm.DB) error {
	if a.ID == "" {
		var err error
		a.ID, err = id.New(id.PrefixHarnessConfig)
		if err != nil {
			return err
		}
	}
	return nil
}

// HarnessConfigSecretBinding binds one of a harness config's environment variables
// to a project secret. Every sandbox created for the harness config materializes
// its bindings into SandboxSecret sentinels, so the harness receives the secret
// values (subject to the usual grant/approval flow — a binding is not a grant).
type HarnessConfigSecretBinding struct {
	ID              string         `gorm:"primaryKey;type:text" json:"id" doc:"Stable binding ID"`
	ProjectID       string         `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID"`
	HarnessConfigID string         `gorm:"column:harness_config_id;not null;type:text;index;uniqueIndex:idx_harness_config_secret_env,priority:1" json:"harnessConfigId" doc:"Harness config the binding belongs to"`
	EnvName         string         `gorm:"column:env_name;not null;type:text;uniqueIndex:idx_harness_config_secret_env,priority:2" json:"envName" doc:"Environment variable filled by the secret"`
	SecretID        string         `gorm:"column:secret_id;not null;type:text;index" json:"secretId" doc:"Bound secret ID"`
	CreatedAt       time.Time      `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt       time.Time      `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`
	DeletedAt       gorm.DeletedAt `gorm:"index" json:"-"`
}

func (HarnessConfigSecretBinding) TableName() string { return "harness_config_secret_bindings" }

func (b *HarnessConfigSecretBinding) EventProjectID() string    { return b.ProjectID }
func (b *HarnessConfigSecretBinding) EventResourceType() string { return "harnessConfigSecretBinding" }
func (b *HarnessConfigSecretBinding) EventResourceID() string   { return b.ID }

func (b *HarnessConfigSecretBinding) BeforeCreate(_ *gorm.DB) error {
	if b.ID == "" {
		var err error
		b.ID, err = id.New(id.PrefixHarnessConfigSecretBinding)
		if err != nil {
			return err
		}
	}
	return nil
}

// HarnessConfigFile is a file to write into a harness's home directory when the
// harness is installed.
type HarnessConfigFile struct {
	Path       string `json:"path" doc:"File path relative to the harness's home directory"`
	Content    string `json:"content" doc:"File content to write"`
	CreateOnly bool   `json:"createOnly,omitempty" doc:"Only create this file if it does not already exist"`
	Template   bool   `json:"template,omitempty" doc:"Render file content against the public sandbox configuration before writing"`
}

// HarnessConfigSecret declares an environment variable the harness expects, and
// whether it is required for the harness to run.
type HarnessConfigSecret struct {
	Name     string `json:"name" doc:"Environment variable name the harness expects to be set" pattern:"^[A-Za-z_][A-Za-z0-9_]*$"`
	Required bool   `json:"required,omitempty" doc:"Whether the secret must be set for the harness to run"`
	// OneOfGroup ties a required secret to a set of alternatives: required secrets
	// sharing a group are satisfied when at least one member is present.
	OneOfGroup string `json:"oneOfGroup,omitempty" doc:"Groups a required secret with alternatives; the requirement is satisfied when at least one member of the group is present"`
}

// GitSource describes a Git source to materialize into a sandbox.
type GitSource struct {
	Kind           string                `json:"kind" doc:"Source kind. Currently only git is supported." enum:"git"`
	Delivery       string                `json:"delivery,omitempty" doc:"How the source reaches the sandbox: clone fetches it from url or localDirectory, push has the client push it into the sandbox's Git repository. Defaults to clone." enum:"clone,push"`
	Slug           *string               `json:"slug,omitempty" doc:"Stable URL-safe source slug used to address the source as a sandbox Git repository"`
	URL            *string               `json:"url,omitempty" doc:"Remote Git source URL" format:"uri"`
	LocalDirectory *string               `json:"localDirectory,omitempty" doc:"Absolute local Git repository directory accessible to the worker"`
	Checkout       *GitSourceCheckout    `json:"checkout,omitempty" doc:"Immutable checkout target and optional user-facing ref identity"`
	Workspace      *GitSourceWorkspace   `json:"workspace,omitempty" doc:"Workspace materialization mode for this source"`
	Destination    *GitSourceDestination `json:"destination,omitempty" doc:"Sandbox destination paths for this source"`
}

// Root returns the normalized identity of the source repository, independent of
// the checked out ref: the absolute local repository root for local sources, and
// the canonical remote URL for remote ones. Sandboxes are grouped by this value
// so a caller sitting in a Git repository can list the sandboxes that run
// against it.
func (s *GitSource) Root() string {
	if s == nil {
		return ""
	}
	if s.LocalDirectory != nil {
		if root := strings.TrimSpace(*s.LocalDirectory); root != "" {
			return root
		}
	}
	if s.URL != nil {
		return strings.TrimSpace(*s.URL)
	}
	return ""
}

// GitSourceCheckout identifies the immutable checkout and user-facing ref.
type GitSourceCheckout struct {
	Commit  *string `json:"commit,omitempty" doc:"Immutable commit SHA to materialize"`
	RefName *string `json:"refName,omitempty" doc:"User-facing branch, tag, or ref name to recreate in the sandbox"`
	RefType *string `json:"refType,omitempty" doc:"User-facing ref type, such as branch, tag, or commit"`
}

// GitSourceWorkspace describes clean or dirty workspace materialization.
type GitSourceWorkspace struct {
	Mode        string  `json:"mode,omitempty" doc:"Whether to materialize a clean checkout or restore dirty workspace changes" enum:"clean,dirty"`
	SnapshotRef *string `json:"snapshotRef,omitempty" doc:"Discobox-owned hidden Git ref containing dirty workspace snapshot data"`
	BaseCommit  *string `json:"baseCommit,omitempty" doc:"Commit to checkout before applying dirty workspace snapshot changes"`
}

// GitSourceDestination describes where a source is placed inside the sandbox.
type GitSourceDestination struct {
	Directory        *string `json:"directory,omitempty" doc:"Directory where this source should be placed inside the sandbox"`
	WorkingDirectory *string `json:"workingDirectory,omitempty" doc:"Working directory inside the sandbox for this source"`
}

// SourceCodeReferences maps sandbox destination directories to Git sources.
type SourceCodeReferences map[string]GitSource

// Origin is the client host and project directory a sandbox was created from.
//
// It is client-declared provenance, recorded verbatim and never used to
// materialize source. It answers "which sandboxes did I start from this
// directory?", which Source cannot: a local path means nothing on another
// machine and collides across hosts and users.
type Origin struct {
	HostID      string `json:"hostId" doc:"Stable generated identity of the client host, unique per user per machine"`
	Hostname    string `json:"hostname,omitempty" doc:"Client hostname, for display only. Not stable and not unique."`
	ProjectPath string `json:"projectPath" doc:"Absolute path of the project root on the client host, which is the Git repository root, or the working directory outside a repository"`
	User        string `json:"user,omitempty" doc:"Client OS username, for display only"`
}

// Key is the indexed identity of an origin, empty when the origin cannot
// identify a client project directory. Clients derive the same value to filter
// listings, so both sides share one implementation.
func (o *Origin) Key() string {
	if o == nil {
		return ""
	}
	return originkey.Of(o.HostID, o.ProjectPath)
}

// Sandbox is the managed runtime/session unit.
type Sandbox struct {
	ID                   string  `gorm:"primaryKey;type:text" json:"id" doc:"Stable sandbox ID"`
	ProjectID            string  `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID"`
	CreatedByUserID      string  `gorm:"column:created_by_user_id;not null;type:text;index" json:"createdByUserId" doc:"Creating user ID"`
	ProviderInstanceID   *string `gorm:"column:provider_instance_id;type:text;index" json:"providerInstanceId,omitempty" doc:"Sandbox provider instance ID"`
	HarnessConfigID      *string `gorm:"column:harness_config_id;type:text;index" json:"harnessConfigId,omitempty" doc:"Harness config ID"`
	HarnessMode          string  `gorm:"column:harness_mode;not null;type:text;default:'run'" json:"harnessMode,omitempty" doc:"Harness startup mode: run or config"`
	Name                 string  `gorm:"not null;type:text" json:"name" doc:"Sandbox name" maxLength:"200"`
	Description          *string `gorm:"type:text" json:"description,omitempty" doc:"Sandbox description"`
	ResourceLifecycle    `gorm:"embedded"`
	RestartGeneration    int64                `gorm:"column:restart_generation;not null;default:0" json:"restartGeneration" doc:"Requested restart generation"`
	RestartedGeneration  int64                `gorm:"column:restarted_generation;not null;default:0" json:"restartedGeneration" doc:"Last restart generation completed by reconciliation"`
	Model                *string              `gorm:"column:model;type:text" json:"model,omitempty" doc:"Model the harness should use"`
	ModelServiceTier     *string              `gorm:"column:model_service_tier;type:text" json:"modelServiceTier,omitempty" doc:"Model service tier the harness should use"`
	ModelReasoningLevel  *string              `gorm:"column:model_reasoning_level;type:text" json:"modelReasoningLevel,omitempty" doc:"Model reasoning level the harness should use"`
	Prompt               []string             `gorm:"column:prompt;type:text;serializer:json" json:"prompt,omitempty" doc:"Prompt the harness should run, passed as argv to preserve the caller's exact tokens"`
	Image                string               `gorm:"column:image;type:text" json:"image,omitempty" doc:"Sandbox base image"`
	Env                  map[string]string    `gorm:"column:env;type:text;serializer:json" json:"env,omitempty" doc:"Environment variables available to sandbox-agent terminals and execs by default"`
	Source               *GitSource           `gorm:"column:source;type:text;serializer:json" json:"source,omitempty" doc:"Primary Git source to materialize in the sandbox"`
	SourceRoot           *string              `gorm:"column:source_root;type:text;index" json:"sourceRoot,omitempty" doc:"Normalized repository identity of the primary source: local repository root path, or remote URL. Derived from Source; used to list the sandboxes belonging to a repository."`
	SourceCodeReferences SourceCodeReferences `gorm:"column:source_code_references;type:text;serializer:json" json:"sourceCodeReferences,omitempty" doc:"Additional Git sources to materialize in the sandbox"`
	Origin               *Origin              `gorm:"column:origin;type:text;serializer:json" json:"origin,omitempty" doc:"Client host and project directory the sandbox was created from. Immutable after create."`
	SourceDeliveredAt    *time.Time           `gorm:"column:source_delivered_at" json:"sourceDeliveredAt,omitempty" doc:"When the client reported its push complete for a push-delivered source. Empty while the sandbox is still awaiting it. The commit to check out is the source's Checkout.Commit, fixed at create." format:"date-time"`
	OriginKey            *string              `gorm:"column:origin_key;type:text;index" json:"-" doc:"Indexed identity of Origin. Derived from Origin; used to list the sandboxes created from one client project directory."`
	UserName             *string              `gorm:"column:user_name;type:text" json:"userName,omitempty" doc:"Username to use inside the sandbox"`
	UserUID              *int                 `gorm:"column:user_uid" json:"userUid,omitempty" doc:"UID to use inside the sandbox"`
	UserGID              *int                 `gorm:"column:user_gid" json:"userGid,omitempty" doc:"GID to use inside the sandbox"`
	HomeDirectory        *string              `gorm:"column:home_directory;type:text" json:"homeDirectory,omitempty" doc:"User home directory to use inside the sandbox"`
	CPUVCPUs             float64              `gorm:"column:cpu_vcpus;not null;default:1" json:"cpuVcpus" doc:"Requested CPU capacity in vCPUs"`
	MemoryBytes          int64                `gorm:"column:memory_bytes;not null;default:0" json:"memoryBytes" doc:"Requested memory capacity in bytes"`
	StorageBytes         int64                `gorm:"column:storage_bytes;not null;default:0" json:"storageBytes" doc:"Requested storage capacity in bytes"`
	WorkerID             *string              `gorm:"column:worker_id;type:text;index" json:"workerId,omitempty" doc:"Assigned worker ID, when scheduled through a worker-backed provider"`
	RuntimeState         json.RawMessage      `gorm:"column:runtime_state;type:text" json:"runtimeState,omitempty" doc:"Non-secret provider runtime state"`
	SecretState          []byte               `gorm:"column:secret_state" json:"-"`
	LastActiveAt         *time.Time           `gorm:"column:last_active_at;index" json:"lastActiveAt,omitempty" doc:"Last observed activity timestamp" format:"date-time"`
	CreatedAt            time.Time            `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt            time.Time            `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`
	DeletedAt            gorm.DeletedAt       `gorm:"index" json:"-"`

	Project          *Project                 `gorm:"foreignKey:ProjectID" json:"-"`
	CreatedBy        *User                    `gorm:"-" json:"createdBy,omitempty" doc:"Creating user"`
	ProviderInstance *SandboxProviderInstance `gorm:"foreignKey:ProviderInstanceID" json:"providerInstance,omitempty" doc:"Sandbox provider instance"`
	HarnessConfig    *HarnessConfig           `gorm:"foreignKey:HarnessConfigID" json:"harnessConfig,omitempty" doc:"Harness config"`
}

func (Sandbox) TableName() string { return "sandboxes" }

func (s *Sandbox) EventProjectID() string { return s.ProjectID }

func (s *Sandbox) EventResourceType() string { return "sandbox" }

func (s *Sandbox) EventResourceID() string { return s.ID }

func (s *Sandbox) BeforeCreate(_ *gorm.DB) error {
	if s.ID == "" {
		var err error
		s.ID, err = id.New(id.PrefixSandbox)
		if err != nil {
			return err
		}
	}
	s.SetDefaults(SandboxDesiredStateRunning, SandboxPhasePending)
	if s.CPUVCPUs <= 0 {
		s.CPUVCPUs = 1
	}
	if s.MemoryBytes < 0 {
		s.MemoryBytes = 0
	}
	if s.StorageBytes < 0 {
		s.StorageBytes = 0
	}
	return nil
}

// SandboxProviderInstance stores configured sandbox provider instances.
//
// Config is for non-secret settings only. Secret material should be encrypted
// into EncryptedConfig or another secret store before this model is persisted.
type SandboxProviderInstance struct {
	ID              string          `gorm:"primaryKey;type:text" json:"id" doc:"Stable provider instance ID"`
	ProjectID       string          `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID"`
	Type            string          `gorm:"column:type;not null;type:text;index" json:"type" doc:"Provider type"`
	Name            string          `gorm:"column:name;not null;type:text" json:"name" doc:"Provider display name" maxLength:"200"`
	Config          json.RawMessage `gorm:"column:config;type:text" json:"config,omitempty" doc:"Non-secret provider configuration"`
	EncryptedConfig []byte          `gorm:"column:encrypted_config" json:"-"`
	BuiltIn         bool            `gorm:"column:built_in;not null;default:false" json:"builtIn" doc:"Whether this provider is built in"`
	Disabled        bool            `gorm:"column:disabled;not null;default:false" json:"disabled" doc:"Whether this provider is disabled"`
	CreatedAt       time.Time       `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt       time.Time       `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`
	DeletedAt       gorm.DeletedAt  `gorm:"index" json:"-"`

	Project   *Project                       `gorm:"foreignKey:ProjectID" json:"-"`
	Sandboxes []Sandbox                      `gorm:"foreignKey:ProviderInstanceID" json:"sandboxes,omitempty" doc:"Sandboxes using this provider"`
	Workers   []Worker                       `gorm:"foreignKey:ProviderInstanceID" json:"workers,omitempty" doc:"Workers using this provider"`
	Status    *SandboxProviderInstanceStatus `gorm:"-" json:"status,omitempty" doc:"Observed provider status derived from persisted worker state"`
}

func (SandboxProviderInstance) TableName() string { return "sandbox_provider_instances" }

func (p *SandboxProviderInstance) BeforeCreate(_ *gorm.DB) error {
	if p.ID == "" {
		var err error
		p.ID, err = id.New(id.PrefixSandboxProvider)
		if err != nil {
			return err
		}
	}
	return nil
}

// SandboxProviderInstanceStatus is observed provider state for display. It is
// derived from persisted worker rows and does not trigger provider-side reads or
// mutations.
type SandboxProviderInstanceStatus struct {
	WorkerCount        int                    `json:"workerCount" doc:"Workers included in the current provider status summary"`
	ReadyWorkers       int                    `json:"readyWorkers" doc:"Workers currently reporting ready"`
	SchedulableWorkers int                    `json:"schedulableWorkers" doc:"Workers currently accepting new sandboxes"`
	DegradedWorkers    int                    `json:"degradedWorkers" doc:"Workers reporting degraded health"`
	FailedWorkers      int                    `json:"failedWorkers" doc:"Workers whose last lifecycle operation failed"`
	LastError          *string                `json:"lastError,omitempty" doc:"Most recent worker error message, if any"`
	Details            json.RawMessage        `json:"details,omitempty" doc:"Provider-specific observed status details"`
	Workers            []ProviderWorkerStatus `json:"workers,omitempty" doc:"Observed worker details"`
}

type ProviderWorkerStatus struct {
	ID                    string     `json:"id" doc:"Worker ID"`
	Identity              string     `json:"identity,omitempty" doc:"Worker identity"`
	DesiredState          string     `json:"desiredState" doc:"Requested worker state"`
	Phase                 string     `json:"phase" doc:"Observed worker lifecycle phase"`
	Ready                 bool       `json:"ready" doc:"Whether the worker is alive and healthy"`
	Schedulable           bool       `json:"schedulable" doc:"Whether the worker accepts new sandboxes"`
	Degraded              bool       `json:"degraded" doc:"Whether the worker is degraded"`
	LastOperationStatus   string     `json:"lastOperationStatus" doc:"Status of the latest worker operation"`
	StatusMessage         *string    `json:"statusMessage,omitempty" doc:"Human-readable status detail"`
	ErrorMessage          *string    `json:"errorMessage,omitempty" doc:"Latest worker error message"`
	AvailableCPUVCPUs     float64    `json:"availableCpuVcpus" doc:"Worker-reported available CPU capacity in vCPUs"`
	AvailableMemoryBytes  int64      `json:"availableMemoryBytes" doc:"Worker-reported available memory capacity in bytes"`
	AvailableStorageBytes int64      `json:"availableStorageBytes" doc:"Worker-reported available storage capacity in bytes"`
	RuntimeID             string     `json:"runtimeId,omitempty" doc:"Sanitized backend runtime ID, such as a VM or container ID"`
	LastSeenAt            *time.Time `json:"lastSeenAt,omitempty" doc:"Last heartbeat timestamp" format:"date-time"`
}

// Worker is a provider-backed runtime worker that can launch sandboxes.
type Worker struct {
	ID                    string          `gorm:"primaryKey;type:text" json:"id" doc:"Stable worker ID"`
	ProjectID             string          `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID"`
	ProviderInstanceID    string          `gorm:"column:provider_instance_id;not null;type:text;index" json:"providerInstanceId" doc:"Sandbox provider instance ID"`
	Identity              string          `gorm:"column:identity;not null;type:text;uniqueIndex" json:"identity" doc:"Worker identity"`
	PublicKey             string          `gorm:"column:public_key;type:text" json:"publicKey,omitempty" doc:"Worker public key"`
	KeyType               string          `gorm:"column:key_type;type:text;default:'ed25519'" json:"keyType,omitempty" doc:"Worker key type"`
	Ready                 bool            `gorm:"column:ready;not null;default:false;index" json:"ready" doc:"Whether the worker is alive and healthy"`
	Schedulable           bool            `gorm:"column:schedulable;not null;default:false;index" json:"schedulable" doc:"Whether the worker is willing to pull new work"`
	Degraded              bool            `gorm:"column:degraded;not null;default:false;index" json:"degraded" doc:"Whether the worker should be used only as fallback capacity"`
	AvailableCPUVCPUs     float64         `gorm:"column:available_cpu_vcpus;not null;default:0;index" json:"availableCpuVcpus" doc:"Worker-reported available CPU capacity in vCPUs"`
	AvailableMemoryBytes  int64           `gorm:"column:available_memory_bytes;not null;default:0;index" json:"availableMemoryBytes" doc:"Worker-reported available memory capacity in bytes"`
	AvailableStorageBytes int64           `gorm:"column:available_storage_bytes;not null;default:0;index" json:"availableStorageBytes" doc:"Worker-reported available storage capacity in bytes"`
	Conditions            json.RawMessage `gorm:"column:conditions;type:text" json:"conditions,omitempty" doc:"Opaque worker-reported condition details for display"`
	RuntimeState          json.RawMessage `gorm:"column:runtime_state;type:text" json:"-" doc:"Internal provider runtime state; may contain boot material and must not be serialized"`
	ResourceLifecycle     `gorm:"embedded"`
	RegisteredAt          *time.Time     `gorm:"column:registered_at" json:"registeredAt,omitempty" doc:"Registration timestamp" format:"date-time"`
	LastSeenAt            *time.Time     `gorm:"column:last_seen_at;index" json:"lastSeenAt,omitempty" doc:"Last heartbeat timestamp" format:"date-time"`
	RevokedAt             *time.Time     `gorm:"column:revoked_at;index" json:"revokedAt,omitempty" doc:"Revocation timestamp" format:"date-time"`
	CreatedAt             time.Time      `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt             time.Time      `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`
	DeletedAt             gorm.DeletedAt `gorm:"index" json:"-"`

	Project          *Project                 `gorm:"foreignKey:ProjectID" json:"-"`
	ProviderInstance *SandboxProviderInstance `gorm:"foreignKey:ProviderInstanceID" json:"providerInstance,omitempty" doc:"Sandbox provider instance"`
	BootstrapTokens  []WorkerBootstrapToken   `gorm:"foreignKey:WorkerID" json:"-" doc:"Worker bootstrap tokens"`
	AuthTokens       []WorkerAuthToken        `gorm:"foreignKey:WorkerID" json:"-" doc:"Legacy worker auth tokens"`
}

func (Worker) TableName() string { return "workers" }

func (w *Worker) EventProjectID() string { return w.ProjectID }

func (w *Worker) EventResourceType() string { return "worker" }

func (w *Worker) EventResourceID() string { return w.ID }

func (w *Worker) BeforeCreate(_ *gorm.DB) error {
	if w.ID == "" {
		var err error
		w.ID, err = id.New(id.PrefixWorker)
		if err != nil {
			return err
		}
	}
	w.SetDefaults(WorkerDesiredStateActive, WorkerPhasePending)
	if w.Identity == "" {
		w.Identity = "worker:" + w.ID
	}
	if w.KeyType == "" {
		w.KeyType = "ed25519"
	}
	return nil
}

// EverCreated reports whether the worker completed its initial create and
// registered at least once. Such workers are stateful (they may own runtime
// state and assigned sandboxes), so a failed reconcile of an already-created
// worker must be reconciled back to health rather than latched to a terminal
// failure. Only a worker that never registered may fail terminally, since its
// initial create never produced anything to recover.
func (w *Worker) EverCreated() bool {
	return w != nil && w.RegisteredAt != nil
}

// SchedulingPreference returns the coarse scheduling bucket for pull-based
// workers. Degraded workers may be used as fallback when preferred workers do
// not claim pending work.
func (w *Worker) SchedulingPreference() string {
	if w == nil || w.RevokedAt != nil || w.DesiredState == WorkerDesiredStateDeleted || w.DesiredState == WorkerDesiredStateDrained {
		return WorkerSchedulingUnavailable
	}
	if !w.Ready || !w.Schedulable {
		return WorkerSchedulingUnavailable
	}
	if w.Degraded {
		return WorkerSchedulingDegraded
	}
	return WorkerSchedulingPreferred
}

// WorkerBootstrapToken stores a short-lived, one-time worker registration token.
// Only the token hash is persisted.
type WorkerBootstrapToken struct {
	ID        string     `gorm:"primaryKey;type:text" json:"id" doc:"Stable bootstrap token ID"`
	WorkerID  string     `gorm:"column:worker_id;not null;type:text;index" json:"workerId" doc:"Worker ID"`
	TokenHash []byte     `gorm:"column:token_hash;not null;uniqueIndex" json:"-"`
	ExpiresAt time.Time  `gorm:"column:expires_at;not null;index" json:"expiresAt" doc:"Expiration timestamp" format:"date-time"`
	UsedAt    *time.Time `gorm:"column:used_at;index" json:"usedAt,omitempty" doc:"Use timestamp" format:"date-time"`
	RevokedAt *time.Time `gorm:"column:revoked_at;index" json:"revokedAt,omitempty" doc:"Revocation timestamp" format:"date-time"`
	CreatedAt time.Time  `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time  `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Worker *Worker `gorm:"foreignKey:WorkerID" json:"-"`
}

func (WorkerBootstrapToken) TableName() string { return "worker_bootstrap_tokens" }

func (t *WorkerBootstrapToken) BeforeCreate(_ *gorm.DB) error {
	if t.ID == "" {
		var err error
		t.ID, err = id.New(id.PrefixWorkerBootstrapToken)
		if err != nil {
			return err
		}
	}
	return nil
}

// WorkerAuthToken stores legacy runtime token metadata for registered workers.
// Active worker runtime auth verifies signed assertions against Worker.PublicKey.
type WorkerAuthToken struct {
	ID         string     `gorm:"primaryKey;type:text" json:"id" doc:"Stable auth token ID"`
	WorkerID   string     `gorm:"column:worker_id;not null;type:text;index" json:"workerId" doc:"Worker ID"`
	TokenHash  []byte     `gorm:"column:token_hash;uniqueIndex" json:"-"`
	IssuedAt   time.Time  `gorm:"column:issued_at;not null;index" json:"issuedAt" doc:"Issue timestamp" format:"date-time"`
	ExpiresAt  time.Time  `gorm:"column:expires_at;not null;index" json:"expiresAt" doc:"Expiration timestamp" format:"date-time"`
	LastUsedAt *time.Time `gorm:"column:last_used_at;index" json:"lastUsedAt,omitempty" doc:"Last use timestamp" format:"date-time"`
	RevokedAt  *time.Time `gorm:"column:revoked_at;index" json:"revokedAt,omitempty" doc:"Revocation timestamp" format:"date-time"`
	CreatedAt  time.Time  `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt  time.Time  `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Worker *Worker `gorm:"foreignKey:WorkerID" json:"-"`
}

func (WorkerAuthToken) TableName() string { return "worker_auth_tokens" }

func (t *WorkerAuthToken) BeforeCreate(_ *gorm.DB) error {
	if t.ID == "" {
		var err error
		t.ID, err = id.New(id.PrefixWorkerAuthToken)
		if err != nil {
			return err
		}
	}
	if t.IssuedAt.IsZero() {
		t.IssuedAt = time.Now().UTC()
	} else {
		t.IssuedAt = t.IssuedAt.UTC()
	}
	if !t.ExpiresAt.IsZero() {
		t.ExpiresAt = t.ExpiresAt.UTC()
	}
	return nil
}

const (
	EventTypeResourceChanged = "resource.changed"
	EventTypeResourceListed  = "resource.listed"

	EventActionCreated = "created"
	EventActionUpdated = "updated"
	EventActionDeleted = "deleted"
	EventActionListed  = "listed"
)

const (
	SecretTypeGit    = "git"
	SecretTypeSSH    = "ssh"
	SecretTypeBearer = "bearer"

	SecretRequestStatusPending  = "pending"
	SecretRequestStatusApproved = "approved"
	SecretRequestStatusDenied   = "denied"

	// SecretGrantScopeSandbox and its siblings decide how widely a single grant
	// applies. A grant is matched against a resolving sandbox by its scope key:
	// the sandbox's own ID, its harness config ID, or the project ID.
	SecretGrantScopeSandbox       = "sandbox"
	SecretGrantScopeHarnessConfig = "harnessConfig"
	SecretGrantScopeProject       = "project"
)

// Secret is a project-scoped encrypted credential that can be requested by sandboxes.
type Secret struct {
	ID              string         `gorm:"primaryKey;type:text" json:"id" doc:"Stable secret ID"`
	ProjectID       string         `gorm:"column:project_id;not null;type:text;index;uniqueIndex:idx_secret_project_type_host,priority:1" json:"projectId" doc:"Project ID"`
	Name            string         `gorm:"column:name;not null;type:text" json:"name" doc:"Secret name"`
	Type            string         `gorm:"column:type;not null;type:text;uniqueIndex:idx_secret_project_type_host,priority:2" json:"type" doc:"Secret type" enum:"git,ssh,bearer"`
	Host            string         `gorm:"column:host;not null;type:text;default:'';uniqueIndex:idx_secret_project_type_host,priority:3" json:"host,omitempty" doc:"Optional host used to match requests"`
	UniqueKey       string         `gorm:"column:unique_key;not null;type:text;default:'';uniqueIndex:idx_secret_project_type_host,priority:4" json:"-"`
	Anonymous       bool           `gorm:"column:anonymous;not null;default:false;index" json:"anonymous,omitempty" doc:"Sandbox-managed secret created from an inline value; referenced only by ID"`
	Format          string         `gorm:"column:format;not null;type:text;default:''" json:"format,omitempty" doc:"Generative format template describing the credential shape; used to mint sentinel placeholders"`
	DefaultGrantTTL int64          `gorm:"column:default_grant_ttl_seconds;not null;default:3600" json:"defaultGrantTTLSeconds" doc:"Default grant duration in seconds"`
	EncryptedValue  []byte         `gorm:"column:encrypted_value" json:"-"`
	DeletedAt       gorm.DeletedAt `gorm:"index" json:"-"`
	CreatedAt       time.Time      `json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt       time.Time      `json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
}

func (Secret) TableName() string { return "secrets" }

func (s *Secret) EventProjectID() string    { return s.ProjectID }
func (s *Secret) EventResourceType() string { return "secret" }
func (s *Secret) EventResourceID() string   { return s.ID }

func (s *Secret) BeforeCreate(_ *gorm.DB) error {
	if s.ID == "" {
		var err error
		s.ID, err = id.New(id.PrefixSecret)
		if err != nil {
			return err
		}
	}
	return nil
}

// SecretValue holds the type-specific plaintext credential fields.
// Only fields relevant to the secret type will be populated.
type SecretValue struct {
	// git
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	// ssh
	PrivateKey string `json:"privateKey,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
	// bearer
	Token string `json:"token,omitempty"`
}

// SecretRequest records a runtime ask for a secret that has no covering grant.
// It is the approval-inbox item: a human resolves it by minting a SecretGrant
// (which approves it) or denying it. Authorization state — who approved, expiry —
// lives on the grant, not here.
type SecretRequest struct {
	ID          string         `gorm:"primaryKey;type:text" json:"id" doc:"Stable request ID"`
	ProjectID   string         `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID"`
	RequestedBy string         `gorm:"column:requested_by;not null;type:text" json:"requestedBy" doc:"Principal ID of the requestor"`
	SandboxID   string         `gorm:"column:sandbox_id;not null;type:text;default:'';index" json:"sandboxId,omitempty" doc:"Sandbox that owns the sentinel, for sandbox-originated requests"`
	Type        string         `gorm:"column:type;not null;type:text" json:"type" doc:"Secret type requested" enum:"git,ssh,bearer"`
	Host        string         `gorm:"column:host;not null;type:text;default:''" json:"host,omitempty" doc:"Host hint provided at request time"`
	SecretID    string         `gorm:"column:secret_id;not null;type:text;default:''" json:"secretId,omitempty" doc:"Matched secret ID; set when approved"`
	Status      string         `gorm:"column:status;not null;type:text;default:'pending'" json:"status" doc:"Request status" enum:"pending,approved,denied"`
	GrantID     string         `gorm:"column:grant_id;not null;type:text;default:''" json:"grantId,omitempty" doc:"Grant that satisfied this request; set when approved"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
	CreatedAt   time.Time      `json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt   time.Time      `json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
}

func (SecretRequest) TableName() string { return "secret_requests" }

func (r *SecretRequest) EventProjectID() string    { return r.ProjectID }
func (r *SecretRequest) EventResourceType() string { return "secretRequest" }
func (r *SecretRequest) EventResourceID() string   { return r.ID }

func (r *SecretRequest) BeforeCreate(_ *gorm.DB) error {
	if r.ID == "" {
		var err error
		r.ID, err = id.New(id.PrefixSecretRequest)
		if err != nil {
			return err
		}
	}
	return nil
}

// SecretGrant is a standing authorization to use a secret, scoped to a sandbox,
// a harness config, or a whole project. It can be minted ahead of any request
// (pre-approval) or as the result of approving a SecretRequest. A live,
// unexpired grant whose scope key matches a resolving sandbox lets the proxy
// return the decrypted value without a pending request.
type SecretGrant struct {
	ID        string         `gorm:"primaryKey;type:text" json:"id" doc:"Stable grant ID"`
	ProjectID string         `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID"`
	SecretID  string         `gorm:"column:secret_id;not null;type:text;index" json:"secretId" doc:"Granted secret ID"`
	Scope     string         `gorm:"column:scope;not null;type:text" json:"scope" doc:"How widely the grant applies" enum:"sandbox,harnessConfig,project"`
	ScopeKey  string         `gorm:"column:scope_key;not null;type:text;index" json:"scopeKey" doc:"Identifier the scope resolves against: sandbox ID, harness config ID, or project ID"`
	Host      string         `gorm:"column:host;not null;type:text;default:''" json:"host,omitempty" doc:"Host the grant is limited to; empty matches any host"`
	GrantedBy string         `gorm:"column:granted_by;not null;type:text;default:''" json:"grantedBy,omitempty" doc:"Principal ID that created the grant"`
	GrantedAt time.Time      `gorm:"column:granted_at;autoCreateTime" json:"grantedAt" doc:"Creation timestamp" format:"date-time"`
	ExpiresAt *time.Time     `gorm:"column:expires_at" json:"expiresAt,omitempty" doc:"Expiry time; empty never expires" format:"date-time"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
	CreatedAt time.Time      `json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time      `json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
}

func (SecretGrant) TableName() string { return "secret_grants" }

func (g *SecretGrant) EventProjectID() string    { return g.ProjectID }
func (g *SecretGrant) EventResourceType() string { return "secretGrant" }
func (g *SecretGrant) EventResourceID() string   { return g.ID }

func (g *SecretGrant) BeforeCreate(_ *gorm.DB) error {
	if g.ID == "" {
		var err error
		g.ID, err = id.New(id.PrefixSecretGrant)
		if err != nil {
			return err
		}
	}
	return nil
}

// SandboxSecretResolution is the result of resolving a sandbox sentinel: either a
// live grant (value populated) or a pending request awaiting approval.
type SandboxSecretResolution struct {
	Status    string
	Value     *SecretValue
	ExpiresAt *time.Time
}

// SandboxSecret binds a sandbox environment variable to a project secret via a
// sentinel placeholder. The sandbox is provisioned with the sentinel value; the
// proxy swaps it for the real value resolved from the referenced secret. The
// sentinel is non-secret but is not exposed through the API.
type SandboxSecret struct {
	ID        string         `gorm:"primaryKey;type:text" json:"id" doc:"Stable assignment ID"`
	ProjectID string         `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID"`
	SandboxID string         `gorm:"column:sandbox_id;not null;type:text;index;uniqueIndex:idx_sandbox_secret_env,priority:1" json:"sandboxId" doc:"Sandbox ID"`
	SecretID  string         `gorm:"column:secret_id;not null;type:text;index" json:"secretId" doc:"Assigned secret ID"`
	EnvName   string         `gorm:"column:env_name;not null;type:text;uniqueIndex:idx_sandbox_secret_env,priority:2" json:"envName" doc:"Environment variable name injected into the sandbox"`
	Sentinel  string         `gorm:"column:sentinel;not null;type:text;uniqueIndex" json:"-"`
	CreatedAt time.Time      `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

func (SandboxSecret) TableName() string { return "sandbox_secrets" }

func (s *SandboxSecret) BeforeCreate(_ *gorm.DB) error {
	if s.ID == "" {
		var err error
		s.ID, err = id.New(id.PrefixSandboxSecret)
		if err != nil {
			return err
		}
	}
	return nil
}

// ProjectEvent is a persisted project-scoped resource change event.
type ProjectEvent struct {
	ID           string          `gorm:"primaryKey;type:text" json:"id" doc:"Event record ID"`
	Seq          int64           `gorm:"column:seq;autoIncrement;uniqueIndex" json:"seq" doc:"Global event sequence" minimum:"0"`
	ProjectID    string          `gorm:"column:project_id;not null;type:text;index:idx_project_event_seq,priority:1" json:"projectId" doc:"Project ID"`
	Type         string          `gorm:"not null;type:text;index" json:"type" doc:"Event type"`
	ResourceType string          `gorm:"column:resource_type;not null;type:text;index" json:"resourceType" doc:"Changed resource type"`
	ResourceID   string          `gorm:"column:resource_id;not null;type:text;index" json:"resourceId" doc:"Changed resource ID"`
	Action       string          `gorm:"not null;type:text;index" json:"action" doc:"Change action" enum:"created,updated,deleted,listed"`
	Data         json.RawMessage `gorm:"type:text;not null" json:"data" doc:"Event payload"`
	CreatedAt    time.Time       `gorm:"autoCreateTime;index:idx_project_event_seq,priority:2" json:"createdAt" doc:"Creation timestamp" format:"date-time"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
}

func (ProjectEvent) TableName() string { return "project_events" }

func (e *ProjectEvent) BeforeCreate(_ *gorm.DB) error {
	if e.ID == "" {
		var err error
		e.ID, err = id.New(id.PrefixEvent)
		if err != nil {
			return err
		}
	}
	return nil
}

// AllModels returns all persisted model types.
func AllModels() []any {
	return []any{
		&User{},
		&Project{},
		&ProjectMember{},
		&ServerState{},
		&SandboxAccessIssuerKey{},
		&HarnessConfig{},
		&HarnessConfigSecretBinding{},
		&Sandbox{},
		&SandboxProviderInstance{},
		&Worker{},
		&WorkerBootstrapToken{},
		&WorkerAuthToken{},
		&ProjectEvent{},
		&Secret{},
		&SecretRequest{},
		&SecretGrant{},
		&SandboxSecret{},
	}
}
