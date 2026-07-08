package handlers

import (
	"context"

	serverapi "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	services "github.com/obot-platform/discobox/server/internal/services"
)

func (h *Handler) ListAgentConfigDefinitions(ctx context.Context) (serverapi.ListAgentConfigDefinitionsRes, error) {
	definitions, err := h.services.AgentConfigs.ListAgentConfigDefinitions(ctx)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListAgentConfigDefinitionsBody](struct {
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
	body, err := services.Convert[apimodel.AgentConfigDefinition](definition)
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
	body, err := services.Convert[apimodel.ListAgentConfigsBody](struct {
		AgentConfigs any `json:"agentConfigs"`
	}{AgentConfigs: configs})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) CreateAgentConfig(ctx context.Context, req *apimodel.CreateAgentConfigBody, params serverapi.CreateAgentConfigParams) (serverapi.CreateAgentConfigRes, error) {
	config, err := h.services.AgentConfigs.CreateAgentConfig(ctx, params.ProjectId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.AgentConfig](config)
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
	body, err := services.Convert[apimodel.AgentConfig](config)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) UpdateAgentConfig(ctx context.Context, req *apimodel.UpdateAgentConfigBody, params serverapi.UpdateAgentConfigParams) (serverapi.UpdateAgentConfigRes, error) {
	config, err := h.services.AgentConfigs.UpdateAgentConfig(ctx, params.ProjectId, params.AgentConfigId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.AgentConfig](config)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) SetDefaultAgentConfig(ctx context.Context, params serverapi.SetDefaultAgentConfigParams) (serverapi.SetDefaultAgentConfigRes, error) {
	project, err := h.services.AgentConfigs.SetDefaultAgentConfig(ctx, params.ProjectId, params.AgentConfigId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Project](project)
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

func (h *Handler) ListAgentConfigSecretBindings(ctx context.Context, params serverapi.ListAgentConfigSecretBindingsParams) (serverapi.ListAgentConfigSecretBindingsRes, error) {
	bindings, err := h.services.AgentConfigs.ListAgentConfigSecretBindings(ctx, params.ProjectId, params.AgentConfigId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListAgentConfigSecretBindingsBody](struct {
		SecretBindings any `json:"secretBindings"`
	}{SecretBindings: bindings})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) SetAgentConfigSecretBinding(ctx context.Context, req *apimodel.SetAgentConfigSecretBindingBody, params serverapi.SetAgentConfigSecretBindingParams) (serverapi.SetAgentConfigSecretBindingRes, error) {
	binding, err := h.services.AgentConfigs.SetAgentConfigSecretBinding(ctx, params.ProjectId, params.AgentConfigId, params.EnvName, req.SecretId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.AgentConfigSecretBinding](binding)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) DeleteAgentConfigSecretBinding(ctx context.Context, params serverapi.DeleteAgentConfigSecretBindingParams) (serverapi.DeleteAgentConfigSecretBindingRes, error) {
	if err := h.services.AgentConfigs.DeleteAgentConfigSecretBinding(ctx, params.ProjectId, params.AgentConfigId, params.EnvName); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DeleteAgentConfigSecretBindingNoContent{}, nil
}
