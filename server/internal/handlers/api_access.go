package handlers

import (
	"context"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

func (h *Handler) CreateAPIAccessRequest(ctx context.Context, req *apimodel.CreateAPIAccessRequestBody, params serverapi.CreateAPIAccessRequestParams) (serverapi.CreateAPIAccessRequestRes, error) {
	result, err := h.services.Sandboxes.CreateAPIAccessRequest(ctx, params.ProjectId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.APIAccessRequest](result)
	return &body, err
}
func (h *Handler) GetAPIAccessRequest(ctx context.Context, params serverapi.GetAPIAccessRequestParams) (serverapi.GetAPIAccessRequestRes, error) {
	result, err := h.services.Sandboxes.GetAPIAccessRequest(ctx, params.ProjectId, params.RequestId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.APIAccessRequest](result)
	return &body, err
}
func (h *Handler) ListAPIAccessRequests(ctx context.Context, params serverapi.ListAPIAccessRequestsParams) (serverapi.ListAPIAccessRequestsRes, error) {
	result, err := h.services.Sandboxes.ListAPIAccessRequests(ctx, params.ProjectId, params.Status.Or(""))
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListAPIAccessRequestsBody](struct {
		APIAccessRequests any `json:"apiAccessRequests"`
	}{result})
	return &body, err
}
func (h *Handler) ApproveAPIAccessRequest(ctx context.Context, params serverapi.ApproveAPIAccessRequestParams) (serverapi.ApproveAPIAccessRequestRes, error) {
	result, err := h.services.Sandboxes.DecideAPIAccessRequest(ctx, params.ProjectId, params.RequestId, true)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.APIAccessRequest](result)
	return &body, err
}
func (h *Handler) DenyAPIAccessRequest(ctx context.Context, params serverapi.DenyAPIAccessRequestParams) (serverapi.DenyAPIAccessRequestRes, error) {
	if _, err := h.services.Sandboxes.DecideAPIAccessRequest(ctx, params.ProjectId, params.RequestId, false); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DenyAPIAccessRequestNoContent{}, nil
}
