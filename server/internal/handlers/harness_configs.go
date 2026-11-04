package handlers

import (
	"context"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// ResetHarnessConfig returns a harness config to its bundle's defaults
// (ADR 26-11-02-152 §3).
func (h *Handler) ResetHarnessConfig(ctx context.Context, params serverapi.ResetHarnessConfigParams) (serverapi.ResetHarnessConfigRes, error) {
	config, err := h.services.HarnessConfigs.ResetHarnessConfig(ctx, params.ProjectId, params.HarnessConfigId, params.IncludeOverrides.Or(false))
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.HarnessConfig](config)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) RefreshHarnessConfigImage(ctx context.Context, params serverapi.RefreshHarnessConfigImageParams) (serverapi.RefreshHarnessConfigImageRes, error) {
	config, err := h.services.HarnessConfigs.RefreshHarnessConfigImage(ctx, params.ProjectId, params.HarnessConfigId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.HarnessConfig](config)
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

func (h *Handler) UnsetDefaultHarnessConfig(ctx context.Context, params serverapi.UnsetDefaultHarnessConfigParams) (serverapi.UnsetDefaultHarnessConfigRes, error) {
	project, err := h.services.HarnessConfigs.UnsetDefaultHarnessConfig(ctx, params.ProjectId, params.HarnessConfigId)
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
