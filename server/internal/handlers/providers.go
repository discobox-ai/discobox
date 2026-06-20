package handlers

import (
	"context"

	serverapi "github.com/obot-platform/discobox/api/servergen"
	services "github.com/obot-platform/discobox/server/internal/services"
)

func (h *Handler) ListSandboxProviderCatalog(ctx context.Context) (serverapi.ListSandboxProviderCatalogRes, error) {
	providers, err := h.services.Providers.ListSandboxProviderCatalogItems(ctx)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[serverapi.ListSandboxProviderCatalogBody](struct {
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
	body, err := services.Convert[serverapi.ListSandboxProviderInstancesBody](struct {
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
	body, err := services.Convert[serverapi.SandboxProviderInstance](provider)
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
	body, err := services.Convert[serverapi.SandboxProviderInstance](provider)
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
	body, err := services.Convert[serverapi.SandboxProviderInstance](provider)
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
