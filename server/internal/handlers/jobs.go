package handlers

import (
	"context"

	serverapi "github.com/obot-platform/discobox/api/servergen"
	services "github.com/obot-platform/discobox/server/internal/services"
)

func (h *Handler) ListJobs(ctx context.Context, params serverapi.ListJobsParams) (serverapi.ListJobsRes, error) {
	jobs, err := h.services.Jobs.ListJobs(ctx, params.ProjectId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[serverapi.ListJobsBody](struct {
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
	body, err := services.Convert[serverapi.Job](job)
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
	body, err := services.Convert[serverapi.Job](job)
	if err != nil {
		return nil, err
	}
	return &body, nil
}
