// Package api defines server service contracts using generated OpenAPI types.
package api

import (
	"context"

	serverapi "github.com/obot-platform/discobox/api/servergen"
	"github.com/obot-platform/discobox/model"
)

type CreateAgentConfigBody = serverapi.CreateAgentConfigBody
type UpdateAgentConfigBody = serverapi.UpdateAgentConfigBody
type CreateSandboxBody = serverapi.CreateSandboxBody
type UpdateSandboxBody = serverapi.UpdateSandboxBody
type StartSandboxBody = serverapi.StartSandboxBody
type StopSandboxBody = serverapi.StopSandboxBody
type RestartSandboxBody = serverapi.RestartSandboxBody
type SandboxProviderCatalogItem = serverapi.SandboxProviderCatalogItem
type CreateSandboxProviderInstanceBody = serverapi.CreateSandboxProviderInstanceBody
type UpdateSandboxProviderInstanceBody = serverapi.UpdateSandboxProviderInstanceBody
type RegisterWorkerBody = serverapi.RegisterWorkerBody
type RegisterWorkerResponseBody = serverapi.RegisterWorkerResponseBody
type UpdateWorkerStatusBody = serverapi.UpdateWorkerStatusBody
type OptString = serverapi.OptString
type OptURI = serverapi.OptURI
type OptInt64 = serverapi.OptInt64
type OptCreateSandboxBodySourceCodeReferences = serverapi.OptCreateSandboxBodySourceCodeReferences

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
