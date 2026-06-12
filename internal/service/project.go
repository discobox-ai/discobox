package service

import (
	"context"
	"time"

	"github.com/obot-platform/discobox/internal/model"
)

// InitializeDefaults creates the single default project used before
// user/project management APIs exist.
func (s *Service) InitializeDefaults(ctx context.Context, tenantID, userID string) error {
	now := time.Now().UTC()
	project := &model.Project{
		ID:          DefaultProjectID,
		TenantID:    tenantID,
		OwnerUserID: userID,
		Name:        "Default Project",
		Slug:        "default",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	return s.store.UpsertProject(ctx, project)
}

func (s *Service) ListProjects(ctx context.Context) ([]model.Project, error) {
	return s.store.ListProjects(ctx)
}

func (s *Service) GetProject(ctx context.Context, projectID string) (*model.Project, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apiError(err, "project not found")
	}
	return project, nil
}
