// Package api registers the public Huma operations for the sandbox manager.
package api

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/obot-platform/discobox/internal/model"
)

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
	RegisterWorker(ctx context.Context, input RegisterWorkerBody) (*RegisterWorkerResponseBody, error)
	UpdateWorkerStatus(ctx context.Context, authorization string, input UpdateWorkerStatusBody) (*model.Worker, error)
}

// ProjectEventService provides project-scoped event replay and live subscription.
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
	Events       ProjectEventService
}

// Register registers all public API operations.
func Register(api huma.API, services Services) {
	RegisterProjectOperations(api, services.Projects)
	RegisterAgentConfigOperations(api, services.AgentConfigs)
	RegisterSandboxProviderInstanceOperations(api, services.Providers)
	RegisterWorkerOperations(api, services.Workers)
	RegisterSandboxOperations(api, services.Sandboxes)
}
