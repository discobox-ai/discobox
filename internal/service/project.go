package service

import (
	"context"
	"time"

	"github.com/obot-platform/disco2/internal/model"
)

// InitializeDefaults creates the single default user and project used before
// user/project management APIs exist.
func (s *Service) InitializeDefaults(ctx context.Context) error {
	now := time.Now().UTC()
	user := &model.User{
		ID:        DefaultUserID,
		Email:     "local@example.com",
		Provider:  "local",
		Subject:   "local",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.store.UpsertUser(ctx, user); err != nil {
		return err
	}

	project := &model.Project{
		ID:          DefaultProjectID,
		OwnerUserID: user.ID,
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
