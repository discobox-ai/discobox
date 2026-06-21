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

func (h *Handler) DeleteAgentConfig(ctx context.Context, params serverapi.DeleteAgentConfigParams) (serverapi.DeleteAgentConfigRes, error) {
	if err := h.services.AgentConfigs.DeleteAgentConfig(ctx, params.ProjectId, params.AgentConfigId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DeleteAgentConfigNoContent{}, nil
}
