package handlers

import (
	"context"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

func (h *Handler) CreateAdoptionRequest(ctx context.Context, req *apimodel.CreateAdoptionRequestBody, params serverapi.CreateAdoptionRequestParams) (serverapi.CreateAdoptionRequestRes, error) {
	result, err := h.services.Sandboxes.CreateAdoptionRequest(ctx, params.ProjectId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.AdoptionRequest](result)
	return &body, err
}
func (h *Handler) GetAdoptionRequest(ctx context.Context, params serverapi.GetAdoptionRequestParams) (serverapi.GetAdoptionRequestRes, error) {
	result, err := h.services.Sandboxes.GetAdoptionRequest(ctx, params.ProjectId, params.RequestId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.AdoptionRequest](result)
	return &body, err
}
func (h *Handler) ListAdoptionRequests(ctx context.Context, params serverapi.ListAdoptionRequestsParams) (serverapi.ListAdoptionRequestsRes, error) {
	result, err := h.services.Sandboxes.ListAdoptionRequests(ctx, params.ProjectId, params.Status.Or(""))
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListAdoptionRequestsBody](struct {
		AdoptionRequests any `json:"adoptionRequests"`
	}{result})
	return &body, err
}
func (h *Handler) ApproveAdoptionRequest(ctx context.Context, params serverapi.ApproveAdoptionRequestParams) (serverapi.ApproveAdoptionRequestRes, error) {
	result, err := h.services.Sandboxes.DecideAdoptionRequest(ctx, params.ProjectId, params.RequestId, true)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.AdoptionRequest](result)
	return &body, err
}
func (h *Handler) DenyAdoptionRequest(ctx context.Context, params serverapi.DenyAdoptionRequestParams) (serverapi.DenyAdoptionRequestRes, error) {
	if _, err := h.services.Sandboxes.DecideAdoptionRequest(ctx, params.ProjectId, params.RequestId, false); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DenyAdoptionRequestNoContent{}, nil
}
func (h *Handler) ReleaseSandbox(ctx context.Context, params serverapi.ReleaseSandboxParams) (serverapi.ReleaseSandboxRes, error) {
	if err := h.services.Sandboxes.ReleaseSandbox(ctx, params.ProjectId, params.SandboxId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.ReleaseSandboxNoContent{}, nil
}
