// Package services defines server service contracts using generated OpenAPI types.
package services

import (
	"context"

	serverapi "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/transport"
)

type ApproveSecretRequestBody = apimodel.ApproveSecretRequestBody
type CreateHarnessConfigBody = apimodel.CreateHarnessConfigBody
type SetHarnessConfigSecretBindingBody = apimodel.SetHarnessConfigSecretBindingBody
type CreateSecretBody = apimodel.CreateSecretBody
type CreateSecretRequestBody = apimodel.CreateSecretRequestBody
type CreateSecretGrantBody = apimodel.CreateSecretGrantBody
type UpdateHarnessConfigBody = apimodel.UpdateHarnessConfigBody
type UpdateSecretBody = apimodel.UpdateSecretBody
type CreateSandboxBody = apimodel.CreateSandboxBody
type CompleteSandboxSourcePushBody = apimodel.CompleteSandboxSourcePushBody
type SandboxSecretInput = apimodel.SandboxSecretInput
type UpdateSandboxBody = apimodel.UpdateSandboxBody
type StartSandboxBody = apimodel.StartSandboxBody
type StopSandboxBody = apimodel.StopSandboxBody
type RestartSandboxBody = apimodel.RestartSandboxBody
type SandboxProviderCatalogItem = apimodel.SandboxProviderCatalogItem
type ProviderConfigField = apimodel.ProviderConfigField
type ProviderStatus = apimodel.ProviderStatus
type CreateSandboxProviderInstanceBody = apimodel.CreateSandboxProviderInstanceBody
type UpdateSandboxProviderInstanceBody = apimodel.UpdateSandboxProviderInstanceBody
type CreatePoolBody = apimodel.CreatePoolBody
type UpdatePoolBody = apimodel.UpdatePoolBody
type RegisterPoolBody = apimodel.RegisterPoolBody
type RegisterPoolResponseBody = apimodel.RegisterPoolResponseBody
type UpdatePoolStatusBody = apimodel.UpdatePoolStatusBody
type ReportPoolSandboxRemovedBody = apimodel.ReportPoolSandboxRemovedBody
type OptBool = serverapi.OptBool
type OptString = serverapi.OptString
type OptURI = serverapi.OptURI
type OptInt64 = serverapi.OptInt64
type OptSandboxUser = serverapi.OptSandboxUser
type OptNilProviderConfigFieldArray = serverapi.OptNilProviderConfigFieldArray
type OptSandboxCreateConfigSourceCodeReferences = serverapi.OptSandboxCreateConfigSourceCodeReferences
type HTTPClientLease = transport.HTTPClientLease

// ProjectService provides read-only access to projects.
type ProjectService interface {
	ListProjects(ctx context.Context) ([]model.Project, error)
	GetProject(ctx context.Context, projectID string) (*model.Project, error)
}

// HarnessConfigService manages project-scoped harness configurations.
type HarnessConfigService interface {
	ListHarnessConfigs(ctx context.Context, projectID string) ([]model.HarnessConfig, error)
	CreateHarnessConfig(ctx context.Context, projectID string, input CreateHarnessConfigBody) (*model.HarnessConfig, error)
	GetHarnessConfig(ctx context.Context, projectID, configID string) (*model.HarnessConfig, error)
	UpdateHarnessConfig(ctx context.Context, projectID, configID string, input UpdateHarnessConfigBody) (*model.HarnessConfig, error)
	SetDefaultHarnessConfig(ctx context.Context, projectID, configID string) (*model.Project, error)
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

	ListHarnessConfigSecretBindings(ctx context.Context, projectID, configID string) ([]model.HarnessConfigSecretBinding, error)
	SetHarnessConfigSecretBinding(ctx context.Context, projectID, configID, envName, secretID string) (*model.HarnessConfigSecretBinding, error)
	DeleteHarnessConfigSecretBinding(ctx context.Context, projectID, configID, envName string) error
}

// SandboxService manages sandboxes within a project.
type SandboxService interface {
	ListSandboxes(ctx context.Context, projectID, sourceRoot, originKey string) ([]model.Sandbox, error)
	CreateSandbox(ctx context.Context, projectID string, input CreateSandboxBody) (*model.Sandbox, error)
	GetSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error)
	UpdateSandbox(ctx context.Context, projectID, sandboxID string, input UpdateSandboxBody) (*model.Sandbox, error)
	DeleteSandbox(ctx context.Context, projectID, sandboxID string) error
	StartSandbox(ctx context.Context, projectID, sandboxID string, input StartSandboxBody) (*model.Sandbox, error)
	StopSandbox(ctx context.Context, projectID, sandboxID string, input StopSandboxBody) (*model.Sandbox, error)
	RestartSandbox(ctx context.Context, projectID, sandboxID string, input RestartSandboxBody) (*model.Sandbox, error)
	CompleteSandboxSourcePush(ctx context.Context, projectID, sandboxID string, input CompleteSandboxSourcePushBody) (*model.Sandbox, error)
	ReconcileSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error)
	AcquireSandboxHTTPClient(ctx context.Context, projectID, sandboxID string, scopes []string) (*HTTPClientLease, *model.Sandbox, error)
	AssignSandboxHarnessSecrets(ctx context.Context, projectID, sandboxID, harnessConfigID string) (map[string]string, error)
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
// pool agent surface: registration, heartbeats, and sandbox-removal reports.
type PoolService interface {
	ListPools(ctx context.Context, projectID string) ([]model.Pool, error)
	CreatePool(ctx context.Context, projectID string, input CreatePoolBody) (*model.Pool, error)
	GetPool(ctx context.Context, projectID, poolID string) (*model.Pool, error)
	UpdatePool(ctx context.Context, projectID, poolID string, input UpdatePoolBody) (*model.Pool, error)
	DeletePool(ctx context.Context, projectID, poolID string) error
	ReconcilePool(ctx context.Context, projectID, poolID string) (*model.Pool, error)

	RegisterPool(ctx context.Context, input RegisterPoolBody) (*RegisterPoolResponseBody, error)
	UpdatePoolStatus(ctx context.Context, poolID string, input UpdatePoolStatusBody) (*model.Pool, error)
	ReportPoolSandboxRemoved(ctx context.Context, poolID string, input ReportPoolSandboxRemovedBody) error
}

// JobService provides project-scoped durable job visibility.
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
}

// ProjectEventService provides project-scoped resource snapshots and live subscription.
type ProjectEventService interface {
	MaxProjectEventSeq(ctx context.Context, projectID string) (int64, error)
	ListProjectEventsAfterSeq(ctx context.Context, projectID string, afterSeq int64, resourceTypes []string) ([]model.ProjectEvent, error)
	ListProjectResourceSnapshots(ctx context.Context, projectID string, resourceTypes []string, seq int64) ([]model.ProjectEvent, error)
	SubscribeProjectEvents(ctx context.Context, projectID string) (<-chan model.ProjectEvent, func(), error)
}

// Services groups the dependencies needed by the API operations.
type Services struct {
	Projects       ProjectService
	HarnessConfigs HarnessConfigService
	Sandboxes      SandboxService
	Providers      SandboxProviderInstanceService
	Pools          PoolService
	Jobs           JobService
	Events         ProjectEventService
	Secrets        SecretService
}
