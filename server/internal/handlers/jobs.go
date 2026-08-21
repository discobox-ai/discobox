package handlers

import (
	"context"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

func (h *Handler) ListJobs(ctx context.Context, params serverapi.ListJobsParams) (serverapi.ListJobsRes, error) {
	jobs, err := h.services.Jobs.ListJobs(ctx, params.ProjectId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListJobsBody](struct {
		Jobs any `json:"jobs"`
	}{Jobs: jobs})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) GetJob(ctx context.Context, params serverapi.GetJobParams) (serverapi.GetJobRes, error) {
	job, err := h.services.Jobs.GetJob(ctx, params.ProjectId, params.JobId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Job](job)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) ForceJob(ctx context.Context, params serverapi.ForceJobParams) (serverapi.ForceJobRes, error) {
	job, err := h.services.Jobs.ForceJob(ctx, params.ProjectId, params.JobId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Job](job)
	if err != nil {
		return nil, err
	}
	return &body, nil
}
