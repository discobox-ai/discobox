// Package model defines the combined database and API resource models.
//
// These structs intentionally carry GORM persistence tags together with JSON
// and Huma/OpenAPI-facing tags. If the API and database shapes diverge later,
// split only the affected resources into DTOs.
package model

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"

	"github.com/obot-platform/disco2/internal/id"
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
		Phase:        WorkerPhaseDeleted,
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

// Tenant is the top-level boundary for users, projects, and their resources.
type Tenant struct {
	ID        string    `gorm:"primaryKey;type:text" json:"id" doc:"Stable tenant ID" format:"uuid"`
	Name      string    `gorm:"not null;type:text" json:"name" doc:"Tenant display name" maxLength:"200"`
	Slug      string    `gorm:"uniqueIndex;not null;type:text" json:"slug" doc:"URL-safe tenant slug" pattern:"^[a-z0-9][a-z0-9-]*$"`
	CreatedAt time.Time `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Users []User `gorm:"foreignKey:TenantID" json:"users,omitempty" doc:"Tenant users"`
}

func (Tenant) TableName() string { return "tenants" }

func (t *Tenant) BeforeCreate(_ *gorm.DB) error {
	if t.ID == "" {
		var err error
		t.ID, err = id.New()
		if err != nil {
			return err
		}
	}
	return nil
}

// User represents an authenticated user.
type User struct {
	ID        string    `gorm:"primaryKey;type:text" json:"id" doc:"Stable user ID" format:"uuid"`
	TenantID  string    `gorm:"column:tenant_id;not null;type:text;index" json:"tenantId" doc:"Tenant ID" format:"uuid"`
	Email     string    `gorm:"uniqueIndex;not null;type:text" json:"email" doc:"User email address" format:"email"`
	Name      *string   `gorm:"type:text" json:"name,omitempty" doc:"Display name"`
	AvatarURL *string   `gorm:"column:avatar_url;type:text" json:"avatarUrl,omitempty" doc:"Avatar image URL" format:"uri"`
	Provider  string    `gorm:"not null;type:text;uniqueIndex:idx_user_provider_subject" json:"provider" doc:"Authentication provider"`
	Subject   string    `gorm:"not null;type:text;uniqueIndex:idx_user_provider_subject" json:"subject" doc:"Provider subject identifier"`
	CreatedAt time.Time `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Tenant *Tenant `gorm:"foreignKey:TenantID" json:"tenant,omitempty" doc:"Tenant"`
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
	ID                       string    `gorm:"primaryKey;type:text" json:"id" doc:"Stable project ID" format:"uuid"`
	TenantID                 string    `gorm:"column:tenant_id;not null;type:text;index" json:"tenantId" doc:"Tenant ID" format:"uuid"`
	OwnerUserID              string    `gorm:"column:owner_user_id;not null;type:text;index" json:"ownerUserId" doc:"Owning user ID" format:"uuid"`
	Name                     string    `gorm:"not null;type:text" json:"name" doc:"Project display name" maxLength:"200"`
	Slug                     string    `gorm:"uniqueIndex;not null;type:text" json:"slug" doc:"URL-safe project slug" pattern:"^[a-z0-9][a-z0-9-]*$"`
	DefaultSandboxProviderID string    `gorm:"column:default_sandbox_provider_id;type:text;default:''" json:"defaultSandboxProviderId,omitempty" doc:"Default sandbox provider instance ID"`
	CreatedAt                time.Time `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt                time.Time `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Tenant                   *Tenant                   `gorm:"-" json:"tenant,omitempty" doc:"Tenant"`
	Owner                    *User                     `gorm:"-" json:"owner,omitempty" doc:"Project owner"`
	Sandboxes                []Sandbox                 `gorm:"foreignKey:ProjectID" json:"sandboxes,omitempty" doc:"Project sandboxes"`
	SandboxProviderInstances []SandboxProviderInstance `gorm:"foreignKey:ProjectID" json:"sandboxProviderInstances,omitempty" doc:"Sandbox provider instances"`
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

// Sandbox is the managed runtime/session unit.
type Sandbox struct {
	ID                  string  `gorm:"primaryKey;type:text" json:"id" doc:"Stable sandbox ID" format:"uuid"`
	ProjectID           string  `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID" format:"uuid"`
	CreatedByUserID     string  `gorm:"column:created_by_user_id;not null;type:text;index" json:"createdByUserId" doc:"Creating user ID" format:"uuid"`
	ProviderInstanceID  *string `gorm:"column:provider_instance_id;type:text;index" json:"providerInstanceId,omitempty" doc:"Sandbox provider instance ID"`
	Name                string  `gorm:"not null;type:text" json:"name" doc:"Sandbox name" maxLength:"200"`
	Description         *string `gorm:"type:text" json:"description,omitempty" doc:"Sandbox description"`
	ResourceLifecycle   `gorm:"embedded"`
	RestartGeneration   int64           `gorm:"column:restart_generation;not null;default:0" json:"restartGeneration" doc:"Requested restart generation"`
	RestartedGeneration int64           `gorm:"column:restarted_generation;not null;default:0" json:"restartedGeneration" doc:"Last restart generation completed by reconciliation"`
	SourceURL           *string         `gorm:"column:source_url;type:text" json:"sourceUrl,omitempty" doc:"Source repository or archive URL" format:"uri"`
	SourceRef           *string         `gorm:"column:source_ref;type:text" json:"sourceRef,omitempty" doc:"Source branch, tag, or commit"`
	WorkingDirectory    *string         `gorm:"column:working_directory;type:text" json:"workingDirectory,omitempty" doc:"Working directory inside the sandbox"`
	CPUVCPUs            float64         `gorm:"column:cpu_vcpus;not null;default:1" json:"cpuVcpus" doc:"Requested CPU capacity in vCPUs"`
	MemoryBytes         int64           `gorm:"column:memory_bytes;not null;default:0" json:"memoryBytes" doc:"Requested memory capacity in bytes"`
	StorageBytes        int64           `gorm:"column:storage_bytes;not null;default:0" json:"storageBytes" doc:"Requested storage capacity in bytes"`
	WorkerID            *string         `gorm:"column:worker_id;type:text;index" json:"workerId,omitempty" doc:"Assigned worker ID, when scheduled through a worker-backed provider"`
	RuntimeState        json.RawMessage `gorm:"column:runtime_state;type:text" json:"runtimeState,omitempty" doc:"Non-secret provider runtime state"`
	SecretState         []byte          `gorm:"column:secret_state" json:"-"`
	LastActiveAt        *time.Time      `gorm:"column:last_active_at;index" json:"lastActiveAt,omitempty" doc:"Last observed activity timestamp" format:"date-time"`
	CreatedAt           time.Time       `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt           time.Time       `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`
	DeletedAt           gorm.DeletedAt  `gorm:"index" json:"-"`

	Project          *Project                 `gorm:"foreignKey:ProjectID" json:"-"`
	CreatedBy        *User                    `gorm:"-" json:"createdBy,omitempty" doc:"Creating user"`
	ProviderInstance *SandboxProviderInstance `gorm:"foreignKey:ProviderInstanceID" json:"providerInstance,omitempty" doc:"Sandbox provider instance"`
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
	ID              string          `gorm:"primaryKey;type:text" json:"id" doc:"Stable provider instance ID" format:"uuid"`
	ProjectID       string          `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID" format:"uuid"`
	Type            string          `gorm:"column:type;not null;type:text;index" json:"type" doc:"Provider type"`
	Name            string          `gorm:"column:name;not null;type:text" json:"name" doc:"Provider display name" maxLength:"200"`
	Config          json.RawMessage `gorm:"column:config;type:text" json:"config,omitempty" doc:"Non-secret provider configuration"`
	EncryptedConfig []byte          `gorm:"column:encrypted_config" json:"-"`
	BuiltIn         bool            `gorm:"column:built_in;not null;default:false" json:"builtIn" doc:"Whether this provider is built in"`
	Disabled        bool            `gorm:"column:disabled;not null;default:false" json:"disabled" doc:"Whether this provider is disabled"`
	CreatedAt       time.Time       `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt       time.Time       `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Project   *Project  `gorm:"foreignKey:ProjectID" json:"-"`
	Sandboxes []Sandbox `gorm:"foreignKey:ProviderInstanceID" json:"sandboxes,omitempty" doc:"Sandboxes using this provider"`
	Workers   []Worker  `gorm:"foreignKey:ProviderInstanceID" json:"workers,omitempty" doc:"Workers using this provider"`
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

// Worker is a provider-backed runtime worker that can launch sandboxes.
type Worker struct {
	ID                    string          `gorm:"primaryKey;type:text" json:"id" doc:"Stable worker ID" format:"uuid"`
	TenantID              string          `gorm:"column:tenant_id;not null;type:text;index" json:"tenantId" doc:"Tenant ID" format:"uuid"`
	ProjectID             string          `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID" format:"uuid"`
	ProviderInstanceID    string          `gorm:"column:provider_instance_id;not null;type:text;index" json:"providerInstanceId" doc:"Sandbox provider instance ID" format:"uuid"`
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
	ResourceLifecycle     `gorm:"embedded"`
	RegisteredAt          *time.Time     `gorm:"column:registered_at" json:"registeredAt,omitempty" doc:"Registration timestamp" format:"date-time"`
	LastSeenAt            *time.Time     `gorm:"column:last_seen_at;index" json:"lastSeenAt,omitempty" doc:"Last heartbeat timestamp" format:"date-time"`
	RevokedAt             *time.Time     `gorm:"column:revoked_at;index" json:"revokedAt,omitempty" doc:"Revocation timestamp" format:"date-time"`
	CreatedAt             time.Time      `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt             time.Time      `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`
	DeletedAt             gorm.DeletedAt `gorm:"index" json:"-"`

	Tenant           *Tenant                  `gorm:"-" json:"tenant,omitempty" doc:"Tenant"`
	Project          *Project                 `gorm:"foreignKey:ProjectID" json:"-"`
	ProviderInstance *SandboxProviderInstance `gorm:"foreignKey:ProviderInstanceID" json:"providerInstance,omitempty" doc:"Sandbox provider instance"`
	BootstrapTokens  []WorkerBootstrapToken   `gorm:"foreignKey:WorkerID" json:"-" doc:"Worker bootstrap tokens"`
	AuthTokens       []WorkerAuthToken        `gorm:"foreignKey:WorkerID" json:"-" doc:"Worker auth tokens"`
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
	ID        string     `gorm:"primaryKey;type:text" json:"id" doc:"Stable bootstrap token ID" format:"uuid"`
	TenantID  string     `gorm:"column:tenant_id;not null;type:text;index" json:"tenantId" doc:"Tenant ID" format:"uuid"`
	WorkerID  string     `gorm:"column:worker_id;not null;type:text;index" json:"workerId" doc:"Worker ID" format:"uuid"`
	TokenHash []byte     `gorm:"column:token_hash;not null;uniqueIndex" json:"-"`
	ExpiresAt time.Time  `gorm:"column:expires_at;not null;index" json:"expiresAt" doc:"Expiration timestamp" format:"date-time"`
	UsedAt    *time.Time `gorm:"column:used_at;index" json:"usedAt,omitempty" doc:"Use timestamp" format:"date-time"`
	RevokedAt *time.Time `gorm:"column:revoked_at;index" json:"revokedAt,omitempty" doc:"Revocation timestamp" format:"date-time"`
	CreatedAt time.Time  `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time  `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Tenant *Tenant `gorm:"-" json:"-"`
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

// WorkerAuthToken stores short-lived runtime token metadata for a registered
// worker. Stateless token implementations can use ID as the token JTI and keep
// this row for revocation/audit, or skip persistence for non-revocable tokens.
type WorkerAuthToken struct {
	ID         string     `gorm:"primaryKey;type:text" json:"id" doc:"Stable auth token ID" format:"uuid"`
	TenantID   string     `gorm:"column:tenant_id;not null;type:text;index" json:"tenantId" doc:"Tenant ID" format:"uuid"`
	WorkerID   string     `gorm:"column:worker_id;not null;type:text;index" json:"workerId" doc:"Worker ID" format:"uuid"`
	TokenHash  []byte     `gorm:"column:token_hash;uniqueIndex" json:"-"`
	IssuedAt   time.Time  `gorm:"column:issued_at;not null;index" json:"issuedAt" doc:"Issue timestamp" format:"date-time"`
	ExpiresAt  time.Time  `gorm:"column:expires_at;not null;index" json:"expiresAt" doc:"Expiration timestamp" format:"date-time"`
	LastUsedAt *time.Time `gorm:"column:last_used_at;index" json:"lastUsedAt,omitempty" doc:"Last use timestamp" format:"date-time"`
	RevokedAt  *time.Time `gorm:"column:revoked_at;index" json:"revokedAt,omitempty" doc:"Revocation timestamp" format:"date-time"`
	CreatedAt  time.Time  `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt  time.Time  `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Tenant *Tenant `gorm:"-" json:"-"`
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
	ID           string          `gorm:"primaryKey;type:text" json:"id" doc:"Stable event ID" format:"uuid"`
	Seq          int64           `gorm:"column:seq;autoIncrement;uniqueIndex" json:"seq" doc:"Global event sequence" minimum:"0"`
	ProjectID    string          `gorm:"column:project_id;not null;type:text;index:idx_project_event_seq,priority:1" json:"projectId" doc:"Project ID" format:"uuid"`
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

// GlobalModels returns model types that live in the global schema/database.
func GlobalModels() []any {
	return []any{
		&Tenant{},
		&User{},
	}
}

// TenantModels returns model types that live in tenant-scoped schemas/databases.
func TenantModels() []any {
	return []any{
		&Project{},
		&SandboxAccessIssuerKey{},
		&Sandbox{},
		&SandboxProviderInstance{},
		&Worker{},
		&WorkerBootstrapToken{},
		&WorkerAuthToken{},
		&ProjectEvent{},
	}
}

// AllModels returns all model types for code paths that intentionally need both
// schema groups. Prefer GlobalModels or TenantModels for migrations.
func AllModels() []any {
	models := append([]any{}, GlobalModels()...)
	return append(models, TenantModels()...)
}
