package handlers

import (
	"context"

	serverapi "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	services "github.com/obot-platform/discobox/server/internal/services"
)

func (h *Handler) ListPools(ctx context.Context, params serverapi.ListPoolsParams) (serverapi.ListPoolsRes, error) {
	pools, err := h.services.Pools.ListPools(ctx, params.ProjectId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListPoolsBody](struct {
		Pools any `json:"pools"`
	}{Pools: pools})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) CreatePool(ctx context.Context, req *apimodel.CreatePoolBody, params serverapi.CreatePoolParams) (serverapi.CreatePoolRes, error) {
	pool, err := h.services.Pools.CreatePool(ctx, params.ProjectId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Pool](pool)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) GetPool(ctx context.Context, params serverapi.GetPoolParams) (serverapi.GetPoolRes, error) {
	pool, err := h.services.Pools.GetPool(ctx, params.ProjectId, params.PoolId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Pool](pool)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) UpdatePool(ctx context.Context, req *apimodel.UpdatePoolBody, params serverapi.UpdatePoolParams) (serverapi.UpdatePoolRes, error) {
	pool, err := h.services.Pools.UpdatePool(ctx, params.ProjectId, params.PoolId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Pool](pool)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) DeletePool(ctx context.Context, params serverapi.DeletePoolParams) (serverapi.DeletePoolRes, error) {
	if err := h.services.Pools.DeletePool(ctx, params.ProjectId, params.PoolId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DeletePoolNoContent{}, nil
}

func (h *Handler) SetDefaultPool(ctx context.Context, params serverapi.SetDefaultPoolParams) (serverapi.SetDefaultPoolRes, error) {
	project, err := h.services.Pools.SetDefaultPool(ctx, params.ProjectId, params.PoolId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Project](project)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) UnsetDefaultPool(ctx context.Context, params serverapi.UnsetDefaultPoolParams) (serverapi.UnsetDefaultPoolRes, error) {
	project, err := h.services.Pools.UnsetDefaultPool(ctx, params.ProjectId, params.PoolId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Project](project)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) ReconcilePool(ctx context.Context, params serverapi.ReconcilePoolParams) (serverapi.ReconcilePoolRes, error) {
	pool, err := h.services.Pools.ReconcilePool(ctx, params.ProjectId, params.PoolId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Pool](pool)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) RegisterPool(ctx context.Context, req *apimodel.RegisterPoolBody) (serverapi.RegisterPoolRes, error) {
	resp, err := h.services.Pools.RegisterPool(ctx, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.RegisterPoolResponseBody](resp)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) UpdatePoolStatus(ctx context.Context, req *apimodel.UpdatePoolStatusBody, params serverapi.UpdatePoolStatusParams) (serverapi.UpdatePoolStatusRes, error) {
	pool, err := h.services.Pools.UpdatePoolStatus(ctx, params.PoolId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Pool](pool)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) ReportPoolSandboxStates(ctx context.Context, req *apimodel.ReportPoolSandboxStatesBody, params serverapi.ReportPoolSandboxStatesParams) (serverapi.ReportPoolSandboxStatesRes, error) {
	if err := h.services.Pools.ReportPoolSandboxStates(ctx, params.PoolId, *req); err != nil {
		return apiError(err), nil
	}
	return &serverapi.ReportPoolSandboxStatesNoContent{}, nil
}
