package generatedapi

import (
	"context"
	"errors"
	"net/http"

	serverapi "github.com/obot-platform/discobox/api/servergen"
	"github.com/obot-platform/discobox/server/internal/api"
)

// Handler adapts server services to the generated OpenAPI server interface.
type Handler struct {
	services api.Services
}

var _ serverapi.Handler = (*Handler)(nil)

// NewHandler creates the generated API handler adapter.
func NewHandler(services api.Services) *Handler {
	return &Handler{services: services}
}

// NewServer creates an http.Handler from the generated OpenAPI server scaffold.
func NewServer(services api.Services, opts ...serverapi.ServerOption) (http.Handler, error) {
	return serverapi.NewServer(NewHandler(services), opts...)
}

func apiError(err error) *serverapi.ErrorModelStatusCode {
	if err == nil {
		return nil
	}
	status := http.StatusInternalServerError
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) {
		status = statusErr.StatusCode()
	}
	return &serverapi.ErrorModelStatusCode{
		StatusCode: status,
		Response: serverapi.ErrorModel{
			Status: serverapi.NewOptInt64(int64(status)),
			Title:  serverapi.NewOptString(http.StatusText(status)),
			Detail: serverapi.NewOptString(err.Error()),
		},
	}
}

func (h *Handler) ListProjects(ctx context.Context) (serverapi.ListProjectsRes, error) {
	projects, err := h.services.Projects.ListProjects(ctx)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.ListProjectsBody](struct {
		Projects any `json:"projects"`
	}{Projects: projects})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) GetProject(ctx context.Context, params serverapi.GetProjectParams) (serverapi.GetProjectRes, error) {
	project, err := h.services.Projects.GetProject(ctx, params.ProjectId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.Project](project)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) ListAgentConfigDefinitions(ctx context.Context) (serverapi.ListAgentConfigDefinitionsRes, error) {
	definitions, err := h.services.AgentConfigs.ListAgentConfigDefinitions(ctx)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.ListAgentConfigDefinitionsBody](struct {
		AgentConfigDefinitions any `json:"agentConfigDefinitions"`
	}{AgentConfigDefinitions: definitions})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) GetAgentConfigDefinition(ctx context.Context, params serverapi.GetAgentConfigDefinitionParams) (serverapi.GetAgentConfigDefinitionRes, error) {
	definition, err := h.services.AgentConfigs.GetAgentConfigDefinition(ctx, params.DefinitionId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.AgentConfigDefinition](definition)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) ListAgentConfigs(ctx context.Context, params serverapi.ListAgentConfigsParams) (serverapi.ListAgentConfigsRes, error) {
	configs, err := h.services.AgentConfigs.ListAgentConfigs(ctx, params.ProjectId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.ListAgentConfigsBody](struct {
		AgentConfigs any `json:"agentConfigs"`
	}{AgentConfigs: configs})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) CreateAgentConfig(ctx context.Context, req *serverapi.CreateAgentConfigBody, params serverapi.CreateAgentConfigParams) (serverapi.CreateAgentConfigRes, error) {
	config, err := h.services.AgentConfigs.CreateAgentConfig(ctx, params.ProjectId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.AgentConfig](config)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) GetAgentConfig(ctx context.Context, params serverapi.GetAgentConfigParams) (serverapi.GetAgentConfigRes, error) {
	config, err := h.services.AgentConfigs.GetAgentConfig(ctx, params.ProjectId, params.AgentConfigId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.AgentConfig](config)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) UpdateAgentConfig(ctx context.Context, req *serverapi.UpdateAgentConfigBody, params serverapi.UpdateAgentConfigParams) (serverapi.UpdateAgentConfigRes, error) {
	config, err := h.services.AgentConfigs.UpdateAgentConfig(ctx, params.ProjectId, params.AgentConfigId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.AgentConfig](config)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) DeleteAgentConfig(ctx context.Context, params serverapi.DeleteAgentConfigParams) (serverapi.DeleteAgentConfigRes, error) {
	if err := h.services.AgentConfigs.DeleteAgentConfig(ctx, params.ProjectId, params.AgentConfigId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DeleteAgentConfigNoContent{}, nil
}

func (h *Handler) ListSandboxes(ctx context.Context, params serverapi.ListSandboxesParams) (serverapi.ListSandboxesRes, error) {
	sandboxes, err := h.services.Sandboxes.ListSandboxes(ctx, params.ProjectId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.ListSandboxesBody](struct {
		Sandboxes any `json:"sandboxes"`
	}{Sandboxes: sandboxes})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) CreateSandbox(ctx context.Context, req *serverapi.CreateSandboxBody, params serverapi.CreateSandboxParams) (serverapi.CreateSandboxRes, error) {
	sandbox, err := h.services.Sandboxes.CreateSandbox(ctx, params.ProjectId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.Sandbox](sandbox)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) GetSandbox(ctx context.Context, params serverapi.GetSandboxParams) (serverapi.GetSandboxRes, error) {
	sandbox, err := h.services.Sandboxes.GetSandbox(ctx, params.ProjectId, params.SandboxId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.Sandbox](sandbox)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) UpdateSandbox(ctx context.Context, req *serverapi.UpdateSandboxBody, params serverapi.UpdateSandboxParams) (serverapi.UpdateSandboxRes, error) {
	sandbox, err := h.services.Sandboxes.UpdateSandbox(ctx, params.ProjectId, params.SandboxId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.Sandbox](sandbox)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) DeleteSandbox(ctx context.Context, params serverapi.DeleteSandboxParams) (serverapi.DeleteSandboxRes, error) {
	if err := h.services.Sandboxes.DeleteSandbox(ctx, params.ProjectId, params.SandboxId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DeleteSandboxAccepted{}, nil
}

func (h *Handler) StartSandbox(ctx context.Context, req *serverapi.StartSandboxBody, params serverapi.StartSandboxParams) (serverapi.StartSandboxRes, error) {
	sandbox, err := h.services.Sandboxes.StartSandbox(ctx, params.ProjectId, params.SandboxId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.Sandbox](sandbox)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) StopSandbox(ctx context.Context, req *serverapi.StopSandboxBody, params serverapi.StopSandboxParams) (serverapi.StopSandboxRes, error) {
	sandbox, err := h.services.Sandboxes.StopSandbox(ctx, params.ProjectId, params.SandboxId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.Sandbox](sandbox)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) RestartSandbox(ctx context.Context, req *serverapi.RestartSandboxBody, params serverapi.RestartSandboxParams) (serverapi.RestartSandboxRes, error) {
	sandbox, err := h.services.Sandboxes.RestartSandbox(ctx, params.ProjectId, params.SandboxId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.Sandbox](sandbox)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) ListSandboxProviderCatalog(ctx context.Context) (serverapi.ListSandboxProviderCatalogRes, error) {
	providers, err := h.services.Providers.ListSandboxProviderCatalogItems(ctx)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.ListSandboxProviderCatalogBody](struct {
		Providers any `json:"providers"`
	}{Providers: providers})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) ListSandboxProviderInstances(ctx context.Context, params serverapi.ListSandboxProviderInstancesParams) (serverapi.ListSandboxProviderInstancesRes, error) {
	providers, err := h.services.Providers.ListSandboxProviderInstances(ctx, params.ProjectId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.ListSandboxProviderInstancesBody](struct {
		Providers any `json:"providers"`
	}{Providers: providers})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) CreateSandboxProviderInstance(ctx context.Context, req *serverapi.CreateSandboxProviderInstanceBody, params serverapi.CreateSandboxProviderInstanceParams) (serverapi.CreateSandboxProviderInstanceRes, error) {
	provider, err := h.services.Providers.CreateSandboxProviderInstance(ctx, params.ProjectId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.SandboxProviderInstance](provider)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) GetSandboxProviderInstance(ctx context.Context, params serverapi.GetSandboxProviderInstanceParams) (serverapi.GetSandboxProviderInstanceRes, error) {
	provider, err := h.services.Providers.GetSandboxProviderInstance(ctx, params.ProjectId, params.ProviderId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.SandboxProviderInstance](provider)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) UpdateSandboxProviderInstance(ctx context.Context, req *serverapi.UpdateSandboxProviderInstanceBody, params serverapi.UpdateSandboxProviderInstanceParams) (serverapi.UpdateSandboxProviderInstanceRes, error) {
	provider, err := h.services.Providers.UpdateSandboxProviderInstance(ctx, params.ProjectId, params.ProviderId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.SandboxProviderInstance](provider)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) DeleteSandboxProviderInstance(ctx context.Context, params serverapi.DeleteSandboxProviderInstanceParams) (serverapi.DeleteSandboxProviderInstanceRes, error) {
	if err := h.services.Providers.DeleteSandboxProviderInstance(ctx, params.ProjectId, params.ProviderId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DeleteSandboxProviderInstanceNoContent{}, nil
}

func (h *Handler) RegisterWorker(ctx context.Context, req *serverapi.RegisterWorkerBody) (serverapi.RegisterWorkerRes, error) {
	resp, err := h.services.Workers.RegisterWorker(ctx, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.RegisterWorkerResponseBody](resp)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) UpdateWorkerStatus(ctx context.Context, req *serverapi.UpdateWorkerStatusBody, params serverapi.UpdateWorkerStatusParams) (serverapi.UpdateWorkerStatusRes, error) {
	worker, err := h.services.Workers.UpdateWorkerStatus(ctx, params.WorkerId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := api.Convert[serverapi.Worker](worker)
	if err != nil {
		return nil, err
	}
	return &body, nil
}
