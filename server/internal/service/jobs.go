package service

import (
	"context"
	"net/http"

	"github.com/obot-platform/discobox/apperrors"
	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/orchestration"
)

func (s *Service) ListJobs(ctx context.Context, projectID string) ([]model.Job, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	jobs, err := s.store.ListJobsForProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]model.Job, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, jobFromOrchestration(job))
	}
	return out, nil
}

func (s *Service) GetJob(ctx context.Context, projectID, jobID string) (*model.Job, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	job, err := s.store.GetJobForProject(ctx, projectID, jobID)
	if err != nil {
		return nil, apiError(err, "job not found")
	}
	out := jobFromOrchestration(*job)
	return &out, nil
}

func (s *Service) ForceJob(ctx context.Context, projectID, jobID string) (*model.Job, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	current, err := s.store.GetJobForProject(ctx, projectID, jobID)
	if err != nil {
		return nil, apiError(err, "job not found")
	}
	if current.Status != orchestration.StatusPending && current.Status != orchestration.StatusBackoff {
		return nil, apperrors.NewStatusError(http.StatusConflict, "job is not pending or in backoff")
	}
	job, err := s.store.ForceJobForProject(ctx, projectID, jobID)
	if err != nil {
		return nil, apiError(err, "job not found")
	}
	if s.notifyNewJob != nil {
		s.notifyNewJob(ctx)
	}
	out := jobFromOrchestration(*job)
	return &out, nil
}

func jobFromOrchestration(job orchestration.Job) model.Job {
	return model.Job{
		ID:           job.ID,
		Type:         string(job.Type),
		Status:       string(job.Status),
		Attempts:     job.Attempts,
		MaxAttempts:  job.MaxAttempts,
		Error:        job.Error,
		Message:      job.Message,
		Metadata:     job.Metadata,
		WorkerID:     job.WorkerID,
		ResourceType: job.Resource.Type,
		ResourceID:   job.Resource.ID,
		ScheduledAt:  job.ScheduledAt,
		StartedAt:    job.StartedAt,
		CompletedAt:  job.CompletedAt,
		CreatedAt:    job.CreatedAt,
		UpdatedAt:    job.UpdatedAt,
	}
}
