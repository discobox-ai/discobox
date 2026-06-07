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
	ID        string    `gorm:"primaryKey;type:text" json:"id" doc:"Stable user ID" format:"uuid"`
	Email     string    `gorm:"uniqueIndex;not null;type:text" json:"email" doc:"User email address" format:"email"`
	Name      *string   `gorm:"type:text" json:"name,omitempty" doc:"Display name"`
	AvatarURL *string   `gorm:"column:avatar_url;type:text" json:"avatarUrl,omitempty" doc:"Avatar image URL" format:"uri"`
	Provider  string    `gorm:"not null;type:text;uniqueIndex:idx_user_provider_subject" json:"provider" doc:"Authentication provider"`
	Subject   string    `gorm:"not null;type:text;uniqueIndex:idx_user_provider_subject" json:"subject" doc:"Provider subject identifier"`
	CreatedAt time.Time `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`
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
	OwnerUserID              string    `gorm:"column:owner_user_id;not null;type:text;index" json:"ownerUserId" doc:"Owning user ID" format:"uuid"`
	Name                     string    `gorm:"not null;type:text" json:"name" doc:"Project display name" maxLength:"200"`
	Slug                     string    `gorm:"uniqueIndex;not null;type:text" json:"slug" doc:"URL-safe project slug" pattern:"^[a-z0-9][a-z0-9-]*$"`
	DefaultSandboxProviderID string    `gorm:"column:default_sandbox_provider_id;type:text;default:''" json:"defaultSandboxProviderId,omitempty" doc:"Default sandbox provider instance ID"`
	CreatedAt                time.Time `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt                time.Time `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Owner                    *User                     `gorm:"foreignKey:OwnerUserID" json:"owner,omitempty" doc:"Project owner"`
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

// ProjectUserKey stores per-project, per-user sandbox trust key material.
type ProjectUserKey struct {
	ProjectID                  string    `gorm:"column:project_id;primaryKey;type:text" json:"-"`
	UserID                     string    `gorm:"column:user_id;primaryKey;type:text" json:"-"`
	SandboxPublicKey           string    `gorm:"column:sandbox_public_key;not null;type:text" json:"-"`
	EncryptedSandboxPrivateKey []byte    `gorm:"column:encrypted_sandbox_private_key;not null" json:"-"`
	CreatedAt                  time.Time `gorm:"autoCreateTime" json:"-"`
	UpdatedAt                  time.Time `gorm:"autoUpdateTime" json:"-"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
	User    *User    `gorm:"foreignKey:UserID" json:"-"`
}

func (ProjectUserKey) TableName() string { return "project_user_keys" }

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
	RuntimeState        json.RawMessage `gorm:"column:runtime_state;type:text" json:"runtimeState,omitempty" doc:"Non-secret provider runtime state"`
	SecretState         []byte          `gorm:"column:secret_state" json:"-"`
	LastActiveAt        *time.Time      `gorm:"column:last_active_at;index" json:"lastActiveAt,omitempty" doc:"Last observed activity timestamp" format:"date-time"`
	CreatedAt           time.Time       `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt           time.Time       `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`
	DeletedAt           gorm.DeletedAt  `gorm:"index" json:"-"`

	Project          *Project                 `gorm:"foreignKey:ProjectID" json:"-"`
	CreatedBy        *User                    `gorm:"foreignKey:CreatedByUserID" json:"createdBy,omitempty" doc:"Creating user"`
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

// AllModels returns all model types for migration.
func AllModels() []any {
	return []any{
		&User{},
		&Project{},
		&ProjectUserKey{},
		&Sandbox{},
		&SandboxProviderInstance{},
		&ProjectEvent{},
	}
}
