// Package services defines server service contracts using generated OpenAPI types.
package services

import (
	"context"

	serverapi "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/transport"
)

type CreateAgentConfigBody = apimodel.CreateAgentConfigBody
type UpdateAgentConfigBody = apimodel.UpdateAgentConfigBody
type CreateSandboxBody = apimodel.CreateSandboxBody
type UpdateSandboxBody = apimodel.UpdateSandboxBody
type StartSandboxBody = apimodel.StartSandboxBody
type StopSandboxBody = apimodel.StopSandboxBody
type RestartSandboxBody = apimodel.RestartSandboxBody
type SandboxProviderCatalogItem = apimodel.SandboxProviderCatalogItem
type ProviderConfigField = apimodel.ProviderConfigField
type ProviderStatus = apimodel.ProviderStatus
type CreateSandboxProviderInstanceBody = apimodel.CreateSandboxProviderInstanceBody
type UpdateSandboxProviderInstanceBody = apimodel.UpdateSandboxProviderInstanceBody
type RegisterWorkerBody = apimodel.RegisterWorkerBody
type RegisterWorkerResponseBody = apimodel.RegisterWorkerResponseBody
type UpdateWorkerStatusBody = apimodel.UpdateWorkerStatusBody
type OptBool = serverapi.OptBool
type OptString = serverapi.OptString
type OptURI = serverapi.OptURI
type OptInt64 = serverapi.OptInt64
type OptSandboxUser = serverapi.OptSandboxUser
type OptNilProviderConfigFieldArray = serverapi.OptNilProviderConfigFieldArray
type OptCreateSandboxBodySourceCodeReferences = serverapi.OptCreateSandboxBodySourceCodeReferences
type HTTPClientLease = transport.HTTPClientLease

// ProjectService provides read-only access to projects.
type ProjectService interface {
	ListProjects(ctx context.Context) ([]model.Project, error)
	GetProject(ctx context.Context, projectID string) (*model.Project, error)
}

// AgentConfigService manages project-scoped agent configurations.
type AgentConfigService interface {
	ListAgentConfigDefinitions(ctx context.Context) ([]model.AgentConfigDefinition, error)
	GetAgentConfigDefinition(ctx context.Context, definitionID string) (*model.AgentConfigDefinition, error)
	ListAgentConfigs(ctx context.Context, projectID string) ([]model.AgentConfig, error)
	CreateAgentConfig(ctx context.Context, projectID string, input CreateAgentConfigBody) (*model.AgentConfig, error)
	GetAgentConfig(ctx context.Context, projectID, configID string) (*model.AgentConfig, error)
	UpdateAgentConfig(ctx context.Context, projectID, configID string, input UpdateAgentConfigBody) (*model.AgentConfig, error)
	DeleteAgentConfig(ctx context.Context, projectID, configID string) error
}

// SandboxService manages sandboxes within a project.
type SandboxService interface {
	ListSandboxes(ctx context.Context, projectID string) ([]model.Sandbox, error)
	CreateSandbox(ctx context.Context, projectID string, input CreateSandboxBody) (*model.Sandbox, error)
	GetSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error)
	UpdateSandbox(ctx context.Context, projectID, sandboxID string, input UpdateSandboxBody) (*model.Sandbox, error)
	DeleteSandbox(ctx context.Context, projectID, sandboxID string) error
	StartSandbox(ctx context.Context, projectID, sandboxID string, input StartSandboxBody) (*model.Sandbox, error)
	StopSandbox(ctx context.Context, projectID, sandboxID string, input StopSandboxBody) (*model.Sandbox, error)
	RestartSandbox(ctx context.Context, projectID, sandboxID string, input RestartSandboxBody) (*model.Sandbox, error)
	ReconcileSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error)
	AcquireSandboxHTTPClient(ctx context.Context, projectID, sandboxID string, scopes []string) (*HTTPClientLease, *model.Sandbox, error)
}

type SandboxProviderInstanceService interface {
	ListSandboxProviderCatalogItems(ctx context.Context) ([]SandboxProviderCatalogItem, error)
	ListSandboxProviderInstances(ctx context.Context, projectID string) ([]model.SandboxProviderInstance, error)
	CreateSandboxProviderInstance(ctx context.Context, projectID string, input CreateSandboxProviderInstanceBody) (*model.SandboxProviderInstance, error)
	GetSandboxProviderInstance(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error)
	UpdateSandboxProviderInstance(ctx context.Context, projectID, providerID string, input UpdateSandboxProviderInstanceBody) (*model.SandboxProviderInstance, error)
	DeleteSandboxProviderInstance(ctx context.Context, projectID, providerID string) error
}

type WorkerService interface {
	ListWorkers(ctx context.Context, projectID, providerID string) ([]model.Worker, error)
	RegisterWorker(ctx context.Context, input RegisterWorkerBody) (*RegisterWorkerResponseBody, error)
	UpdateWorkerStatus(ctx context.Context, workerID string, input UpdateWorkerStatusBody) (*model.Worker, error)
	ReconcileWorker(ctx context.Context, projectID, workerID string) (*model.Worker, error)
}

// JobService provides project-scoped durable job visibility.
type JobService interface {
	GetJob(ctx context.Context, projectID, jobID string) (*model.Job, error)
	ForceJob(ctx context.Context, projectID, jobID string) (*model.Job, error)
	ListJobs(ctx context.Context, projectID string) ([]model.Job, error)
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
	Projects     ProjectService
	AgentConfigs AgentConfigService
	Sandboxes    SandboxService
	Providers    SandboxProviderInstanceService
	Workers      WorkerService
	Jobs         JobService
	Events       ProjectEventService
}
