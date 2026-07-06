// Package model defines the combined database and API resource models.
//
// These structs intentionally carry GORM persistence tags together with JSON
// and OpenAPI-facing tags. If the API and database shapes diverge later,
// split only the affected resources into DTOs.
package model

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/id"
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
	SandboxPhaseStarting     = "starting"
	SandboxPhaseRunning      = "running"
	SandboxPhaseStopping     = "stopping"
	SandboxPhaseStopped      = "stopped"
	SandboxPhaseDeleting     = "deleting"
	SandboxPhaseDeleted      = "deleted"
	SandboxPhaseFailed       = "failed"

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
		u.ID, err = id.New()
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
	DefaultAgentConfigID     string         `gorm:"column:default_agent_config_id;type:text;default:''" json:"defaultAgentConfigId,omitempty" doc:"Default agent config ID"`
	CreatedAt                time.Time      `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt                time.Time      `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`
	DeletedAt                gorm.DeletedAt `gorm:"index" json:"-"`

	Owner                    *User                     `gorm:"-" json:"owner,omitempty" doc:"Project owner"`
	Members                  []ProjectMember           `gorm:"foreignKey:ProjectID" json:"members,omitempty" doc:"Project members"`
	Sandboxes                []Sandbox                 `gorm:"foreignKey:ProjectID" json:"sandboxes,omitempty" doc:"Project sandboxes"`
	SandboxProviderInstances []SandboxProviderInstance `gorm:"foreignKey:ProjectID" json:"sandboxProviderInstances,omitempty" doc:"Sandbox provider instances"`
	AgentConfigs             []AgentConfig             `gorm:"foreignKey:ProjectID" json:"agentConfigs,omitempty" doc:"Agent configurations"`
}

func (Project) TableName() string { return "projects" }

func (p *Project) BeforeCreate(_ *gorm.DB) error {
	if p.ID == "" {
		var err error
		p.ID, err = id.New()
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

// AgentConfig stores a project-scoped agent runtime configuration.
type AgentConfig struct {
	ID             string            `gorm:"primaryKey;type:text" json:"id" doc:"Stable agent config ID"`
	ProjectID      string            `gorm:"column:project_id;not null;type:text;index;uniqueIndex:idx_agent_config_project_name,priority:1" json:"projectId" doc:"Project ID"`
	Name           string            `gorm:"column:name;not null;type:text;uniqueIndex:idx_agent_config_project_name,priority:2" json:"name" doc:"Agent config name" maxLength:"200"`
	InstallCommand []string          `gorm:"column:install_command;type:text;serializer:json" json:"installCommand,omitempty" doc:"Argv used to install the agent. Not run through a shell; use [\"sh\", \"-c\", \"...\"] for shell semantics."`
	RunCommand     []string          `gorm:"column:run_command;not null;type:text;serializer:json" json:"runCommand" doc:"Argv used to run the agent. Not run through a shell; use [\"sh\", \"-c\", \"...\"] for shell semantics."`
	Files          []AgentConfigFile `gorm:"column:files;type:text;serializer:json" json:"files,omitempty" doc:"Files to write into the agent's home directory when the agent is installed"`
	CreatedAt      time.Time         `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt      time.Time         `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Project   *Project  `gorm:"foreignKey:ProjectID" json:"-"`
	Sandboxes []Sandbox `gorm:"foreignKey:AgentConfigID" json:"-"`
}

func (AgentConfig) TableName() string { return "agent_configs" }

func (a *AgentConfig) EventProjectID() string { return a.ProjectID }

func (a *AgentConfig) EventResourceType() string { return "agentConfig" }

func (a *AgentConfig) EventResourceID() string { return a.ID }

func (a *AgentConfig) BeforeCreate(_ *gorm.DB) error {
	if a.ID == "" {
		var err error
		a.ID, err = id.New()
		if err != nil {
			return err
		}
	}
	return nil
}

// AgentConfigDefinition is a well-known template for creating an AgentConfig.
//
// Definitions are not project-scoped AgentConfig instances and cannot be
// selected by sandboxes directly. They provide UI-visible defaults for creating
// real AgentConfig records.
type AgentConfigDefinition struct {
	ID             string            `json:"id" doc:"Stable definition ID"`
	Name           string            `json:"name" doc:"Agent config definition name" maxLength:"200"`
	Description    string            `json:"description,omitempty" doc:"Agent config definition description"`
	InstallCommand []string          `json:"installCommand,omitempty" doc:"Argv used to install the agent. Not run through a shell; use [\"sh\", \"-c\", \"...\"] for shell semantics."`
	RunCommand     []string          `json:"runCommand" doc:"Argv used to run the agent. Not run through a shell; use [\"sh\", \"-c\", \"...\"] for shell semantics."`
	Files          []AgentConfigFile `json:"files,omitempty" doc:"Files to write into the agent's home directory when the agent is installed"`
}

// AgentConfigFile is a file to write into an agent's home directory when the
// agent is installed.
type AgentConfigFile struct {
	Path    string `json:"path" doc:"File path relative to the agent's home directory"`
	Content string `json:"content" doc:"File content to write"`
}

// GitSource describes a Git source to materialize into a sandbox.
type GitSource struct {
	Kind           string                `json:"kind" doc:"Source kind. Currently only git is supported." enum:"git"`
	Slug           *string               `json:"slug,omitempty" doc:"Stable URL-safe source slug used to address the source as a sandbox Git repository"`
	URL            *string               `json:"url,omitempty" doc:"Remote Git source URL" format:"uri"`
	LocalDirectory *string               `json:"localDirectory,omitempty" doc:"Absolute local Git repository directory accessible to the worker"`
	Checkout       *GitSourceCheckout    `json:"checkout,omitempty" doc:"Immutable checkout target and optional user-facing ref identity"`
	Workspace      *GitSourceWorkspace   `json:"workspace,omitempty" doc:"Workspace materialization mode for this source"`
	Destination    *GitSourceDestination `json:"destination,omitempty" doc:"Sandbox destination paths for this source"`
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

// Sandbox is the managed runtime/session unit.
type Sandbox struct {
	ID                       string  `gorm:"primaryKey;type:text" json:"id" doc:"Stable sandbox ID"`
	ProjectID                string  `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID"`
	CreatedByUserID          string  `gorm:"column:created_by_user_id;not null;type:text;index" json:"createdByUserId" doc:"Creating user ID"`
	ProviderInstanceID       *string `gorm:"column:provider_instance_id;type:text;index" json:"providerInstanceId,omitempty" doc:"Sandbox provider instance ID"`
	AgentConfigID            *string `gorm:"column:agent_config_id;type:text;index" json:"agentConfigId,omitempty" doc:"Agent config ID"`
	Name                     string  `gorm:"not null;type:text" json:"name" doc:"Sandbox name" maxLength:"200"`
	Description              *string `gorm:"type:text" json:"description,omitempty" doc:"Sandbox description"`
	ResourceLifecycle        `gorm:"embedded"`
	RestartGeneration        int64                `gorm:"column:restart_generation;not null;default:0" json:"restartGeneration" doc:"Requested restart generation"`
	RestartedGeneration      int64                `gorm:"column:restarted_generation;not null;default:0" json:"restartedGeneration" doc:"Last restart generation completed by reconciliation"`
	AgentModel               *string              `gorm:"column:agent_model;type:text" json:"agentModel,omitempty" doc:"Model the agent should use"`
	AgentModelServiceTier    *string              `gorm:"column:agent_model_service_tier;type:text" json:"agentModelServiceTier,omitempty" doc:"Model service tier the agent should use"`
	AgentModelReasoningLevel *string              `gorm:"column:agent_model_reasoning_level;type:text" json:"agentModelReasoningLevel,omitempty" doc:"Model reasoning level the agent should use"`
	Prompt                   *string              `gorm:"column:prompt;type:text" json:"prompt,omitempty" doc:"Prompt the agent should run"`
	Image                    string               `gorm:"column:image;type:text" json:"image,omitempty" doc:"Sandbox base image"`
	Env                      map[string]string    `gorm:"column:env;type:text;serializer:json" json:"env,omitempty" doc:"Environment variables available to sandbox-agent terminals and execs by default"`
	Source                   *GitSource           `gorm:"column:source;type:text;serializer:json" json:"source,omitempty" doc:"Primary Git source to materialize in the sandbox"`
	SourceCodeReferences     SourceCodeReferences `gorm:"column:source_code_references;type:text;serializer:json" json:"sourceCodeReferences,omitempty" doc:"Additional Git sources to materialize in the sandbox"`
	UserName                 *string              `gorm:"column:user_name;type:text" json:"userName,omitempty" doc:"Username to use inside the sandbox"`
	UserUID                  *int                 `gorm:"column:user_uid" json:"userUid,omitempty" doc:"UID to use inside the sandbox"`
	UserGID                  *int                 `gorm:"column:user_gid" json:"userGid,omitempty" doc:"GID to use inside the sandbox"`
	HomeDirectory            *string              `gorm:"column:home_directory;type:text" json:"homeDirectory,omitempty" doc:"User home directory to use inside the sandbox"`
	CPUVCPUs                 float64              `gorm:"column:cpu_vcpus;not null;default:1" json:"cpuVcpus" doc:"Requested CPU capacity in vCPUs"`
	MemoryBytes              int64                `gorm:"column:memory_bytes;not null;default:0" json:"memoryBytes" doc:"Requested memory capacity in bytes"`
	StorageBytes             int64                `gorm:"column:storage_bytes;not null;default:0" json:"storageBytes" doc:"Requested storage capacity in bytes"`
	WorkerID                 *string              `gorm:"column:worker_id;type:text;index" json:"workerId,omitempty" doc:"Assigned worker ID, when scheduled through a worker-backed provider"`
	RuntimeState             json.RawMessage      `gorm:"column:runtime_state;type:text" json:"runtimeState,omitempty" doc:"Non-secret provider runtime state"`
	SecretState              []byte               `gorm:"column:secret_state" json:"-"`
	LastActiveAt             *time.Time           `gorm:"column:last_active_at;index" json:"lastActiveAt,omitempty" doc:"Last observed activity timestamp" format:"date-time"`
	CreatedAt                time.Time            `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt                time.Time            `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`
	DeletedAt                gorm.DeletedAt       `gorm:"index" json:"-"`

	Project          *Project                 `gorm:"foreignKey:ProjectID" json:"-"`
	CreatedBy        *User                    `gorm:"-" json:"createdBy,omitempty" doc:"Creating user"`
	ProviderInstance *SandboxProviderInstance `gorm:"foreignKey:ProviderInstanceID" json:"providerInstance,omitempty" doc:"Sandbox provider instance"`
	AgentConfig      *AgentConfig             `gorm:"foreignKey:AgentConfigID" json:"agentConfig,omitempty" doc:"Agent config"`
}

func (Sandbox) TableName() string { return "sandboxes" }

func (s *Sandbox) EventProjectID() string { return s.ProjectID }

func (s *Sandbox) EventResourceType() string { return "sandbox" }

func (s *Sandbox) EventResourceID() string { return s.ID }

func (s *Sandbox) BeforeCreate(_ *gorm.DB) error {
	if s.ID == "" {
		var err error
		s.ID, err = id.New()
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
		p.ID, err = id.New()
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
		w.ID, err = id.New()
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
		t.ID, err = id.New()
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
		t.ID, err = id.New()
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
		e.ID, err = id.New()
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
		&AgentConfig{},
		&Sandbox{},
		&SandboxProviderInstance{},
		&Worker{},
		&WorkerBootstrapToken{},
		&WorkerAuthToken{},
		&ProjectEvent{},
	}
}
