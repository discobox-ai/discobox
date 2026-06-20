package handlers

import (
	"context"

	serverapi "github.com/obot-platform/discobox/api/servergen"
	services "github.com/obot-platform/discobox/server/internal/services"
)

func (h *Handler) ListWorkers(ctx context.Context, params serverapi.ListWorkersParams) (serverapi.ListWorkersRes, error) {
	workers, err := h.services.Workers.ListWorkers(ctx, params.ProjectId, params.Provider.Or(""))
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[serverapi.ListWorkersBody](struct {
		Workers any `json:"workers"`
	}{Workers: workers})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) RegisterWorker(ctx context.Context, req *serverapi.RegisterWorkerBody) (serverapi.RegisterWorkerRes, error) {
	resp, err := h.services.Workers.RegisterWorker(ctx, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[serverapi.RegisterWorkerResponseBody](resp)
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
	body, err := services.Convert[serverapi.Worker](worker)
	if err != nil {
		return nil, err
	}
	return &body, nil
}
