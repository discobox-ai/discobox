// Package jobs also serves the jobs REST API. Job rows no longer exist: the
// API is backed by the reconcile engine's dirty set. A "job" is a pending
// reconcile mark; terminal history lives on the resources themselves
// (operation status) and in project events.
package jobs

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/reconcile"
	"github.com/obot-platform/discobox/server/internal/resources/sandboxes"
	"github.com/obot-platform/discobox/server/internal/resources/workers"
	"github.com/obot-platform/discobox/server/internal/store"
)

type Service struct {
	store  *store.Store
	engine *reconcile.Engine
}

func NewService(store *store.Store, engine *reconcile.Engine) *Service {
	return &Service{store: store, engine: engine}
}

// apiJobID encodes a dirty mark as an opaque, path-safe job id:
// "type:resource-id" with "/" flattened to ":" (ids themselves contain
// neither).
func apiJobID(mark reconcile.DirtyResource) string {
	return strings.ReplaceAll(mark.ResourceType+"/"+mark.ResourceID, "/", ":")
}

func parseAPIJobID(id string) (resourceType, resourceID string, err error) {
	parts := strings.Split(id, ":")
	if len(parts) < 2 || parts[0] == "" {
		return "", "", apperrors.NewStatusError(http.StatusNotFound, "job not found")
	}
	return parts[0], strings.Join(parts[1:], "/"), nil
}

func (s *Service) getEngine() (*reconcile.Engine, error) {
	if s == nil || s.engine == nil {
		return nil, errors.New("reconcile engine is required")
	}
	return s.engine, nil
}

// ListJobs returns the project's pending reconcile marks.
func (s *Service) ListJobs(ctx context.Context, projectID string) ([]model.Job, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	engine, err := s.getEngine()
	if err != nil {
		return nil, err
	}
	marks, err := engine.ListDirty(ctx)
	if err != nil {
		return nil, err
	}
	projectWorkers, err := s.projectWorkerIDs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]model.Job, 0, len(marks))
	for _, mark := range marks {
		if !markInProject(mark, projectID, projectWorkers) {
			continue
		}
		out = append(out, jobFromMark(mark))
	}
	return out, nil
}

func (s *Service) GetJob(ctx context.Context, projectID, jobID string) (*model.Job, error) {
	mark, err := s.findMark(ctx, projectID, jobID)
	if err != nil {
		return nil, err
	}
	job := jobFromMark(*mark)
	return &job, nil
}

// ForceJob pulls a pending/backoff mark forward so it is claimable now.
func (s *Service) ForceJob(ctx context.Context, projectID, jobID string) (*model.Job, error) {
	mark, err := s.findMark(ctx, projectID, jobID)
	if err != nil {
		return nil, err
	}
	if mark.ClaimedBy != nil {
		return nil, apperrors.NewStatusError(http.StatusConflict, "job is not pending or in backoff")
	}
	engine, err := s.getEngine()
	if err != nil {
		return nil, err
	}
	if err := engine.MarkDirty(ctx, mark.ResourceType, mark.ResourceID); err != nil {
		return nil, err
	}
	forced := *mark
	forced.NotBefore = time.Now()
	job := jobFromMark(forced)
	return &job, nil
}

func (s *Service) findMark(ctx context.Context, projectID, jobID string) (*reconcile.DirtyResource, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	resourceType, resourceID, err := parseAPIJobID(jobID)
	if err != nil {
		return nil, err
	}
	engine, err := s.getEngine()
	if err != nil {
		return nil, err
	}
	marks, err := engine.ListDirty(ctx, resourceType)
	if err != nil {
		return nil, err
	}
	projectWorkers, err := s.projectWorkerIDs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for _, mark := range marks {
		if mark.ResourceID == resourceID && markInProject(mark, projectID, projectWorkers) {
			return &mark, nil
		}
	}
	return nil, apperrors.NewStatusError(http.StatusNotFound, "job not found")
}

func (s *Service) projectWorkerIDs(ctx context.Context, projectID string) (map[string]struct{}, error) {
	list, err := s.store.ListWorkers(ctx, projectID, "")
	if err != nil {
		return nil, err
	}
	ids := make(map[string]struct{}, len(list))
	for i := range list {
		ids[list[i].ID] = struct{}{}
	}
	return ids, nil
}

// markInProject scopes marks to a project: sandbox and workerprovider ids
// carry a "projectID/" prefix; worker ids resolve through the project's
// worker set.
func markInProject(mark reconcile.DirtyResource, projectID string, projectWorkers map[string]struct{}) bool {
	switch mark.ResourceType {
	case sandboxes.SandboxResourceType, workers.WorkerProviderResourceType:
		return strings.HasPrefix(mark.ResourceID, projectID+"/")
	case workers.WorkerResourceType:
		_, ok := projectWorkers[mark.ResourceID]
		return ok
	default:
		return false
	}
}

func jobFromMark(mark reconcile.DirtyResource) model.Job {
	status := "pending"
	switch {
	case mark.ClaimedBy != nil:
		status = "running"
	case mark.NotBefore.After(time.Now()):
		status = "backoff"
	}
	resourceID := mark.ResourceID
	if idx := strings.LastIndex(resourceID, "/"); idx >= 0 {
		resourceID = resourceID[idx+1:]
	}
	return model.Job{
		ID:           apiJobID(mark),
		Type:         mark.ResourceType + ".reconcile",
		Status:       status,
		Attempts:     mark.Attempts,
		WorkerID:     mark.ClaimedBy,
		ResourceType: mark.ResourceType,
		ResourceID:   resourceID,
		ScheduledAt:  mark.NotBefore,
		CreatedAt:    mark.MarkedAt,
		UpdatedAt:    mark.MarkedAt,
	}
}

func apiError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return apperrors.NewStatusError(http.StatusNotFound, notFoundMessage)
	}
	return err
}
