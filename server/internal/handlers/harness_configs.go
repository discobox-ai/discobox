package handlers

import (
	"context"

	serverapi "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	services "github.com/obot-platform/discobox/server/internal/services"
)

func (h *Handler) ListHarnessDefinitions(ctx context.Context) (serverapi.ListHarnessDefinitionsRes, error) {
	definitions, err := h.services.HarnessConfigs.ListHarnessDefinitions(ctx)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListHarnessDefinitionsBody](struct {
		HarnessDefinitions any `json:"harnessDefinitions"`
	}{HarnessDefinitions: definitions})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) GetHarnessDefinition(ctx context.Context, params serverapi.GetHarnessDefinitionParams) (serverapi.GetHarnessDefinitionRes, error) {
	definition, err := h.services.HarnessConfigs.GetHarnessDefinition(ctx, params.DefinitionId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.HarnessDefinition](definition)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) ListHarnessConfigs(ctx context.Context, params serverapi.ListHarnessConfigsParams) (serverapi.ListHarnessConfigsRes, error) {
	configs, err := h.services.HarnessConfigs.ListHarnessConfigs(ctx, params.ProjectId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListHarnessConfigsBody](struct {
		HarnessConfigs any `json:"harnessConfigs"`
	}{HarnessConfigs: configs})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) CreateHarnessConfig(ctx context.Context, req *apimodel.CreateHarnessConfigBody, params serverapi.CreateHarnessConfigParams) (serverapi.CreateHarnessConfigRes, error) {
	config, err := h.services.HarnessConfigs.CreateHarnessConfig(ctx, params.ProjectId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.HarnessConfig](config)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) GetHarnessConfig(ctx context.Context, params serverapi.GetHarnessConfigParams) (serverapi.GetHarnessConfigRes, error) {
	config, err := h.services.HarnessConfigs.GetHarnessConfig(ctx, params.ProjectId, params.HarnessConfigId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.HarnessConfig](config)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) UpdateHarnessConfig(ctx context.Context, req *apimodel.UpdateHarnessConfigBody, params serverapi.UpdateHarnessConfigParams) (serverapi.UpdateHarnessConfigRes, error) {
	config, err := h.services.HarnessConfigs.UpdateHarnessConfig(ctx, params.ProjectId, params.HarnessConfigId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.HarnessConfig](config)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) SetDefaultHarnessConfig(ctx context.Context, params serverapi.SetDefaultHarnessConfigParams) (serverapi.SetDefaultHarnessConfigRes, error) {
	project, err := h.services.HarnessConfigs.SetDefaultHarnessConfig(ctx, params.ProjectId, params.HarnessConfigId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Project](project)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) DeleteHarnessConfig(ctx context.Context, params serverapi.DeleteHarnessConfigParams) (serverapi.DeleteHarnessConfigRes, error) {
	if err := h.services.HarnessConfigs.DeleteHarnessConfig(ctx, params.ProjectId, params.HarnessConfigId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DeleteHarnessConfigNoContent{}, nil
}

func (h *Handler) ListHarnessConfigSecretBindings(ctx context.Context, params serverapi.ListHarnessConfigSecretBindingsParams) (serverapi.ListHarnessConfigSecretBindingsRes, error) {
	bindings, err := h.services.HarnessConfigs.ListHarnessConfigSecretBindings(ctx, params.ProjectId, params.HarnessConfigId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListHarnessConfigSecretBindingsBody](struct {
		SecretBindings any `json:"secretBindings"`
	}{SecretBindings: bindings})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) SetHarnessConfigSecretBinding(ctx context.Context, req *apimodel.SetHarnessConfigSecretBindingBody, params serverapi.SetHarnessConfigSecretBindingParams) (serverapi.SetHarnessConfigSecretBindingRes, error) {
	binding, err := h.services.HarnessConfigs.SetHarnessConfigSecretBinding(ctx, params.ProjectId, params.HarnessConfigId, params.EnvName, req.SecretId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.HarnessConfigSecretBinding](binding)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) DeleteHarnessConfigSecretBinding(ctx context.Context, params serverapi.DeleteHarnessConfigSecretBindingParams) (serverapi.DeleteHarnessConfigSecretBindingRes, error) {
	if err := h.services.HarnessConfigs.DeleteHarnessConfigSecretBinding(ctx, params.ProjectId, params.HarnessConfigId, params.EnvName); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DeleteHarnessConfigSecretBindingNoContent{}, nil
}
