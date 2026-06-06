package store

import (
	"context"

	"github.com/obot-platform/disco2/internal/model"
)

func (s *Store) UpsertProject(ctx context.Context, project *model.Project) error {
	return s.write.WithContext(ctx).Save(project).Error
}

func (s *Store) ListProjects(ctx context.Context) ([]model.Project, error) {
	var projects []model.Project
	err := s.read.WithContext(ctx).
		Preload("Owner").
		Order("created_at ASC").
		Find(&projects).Error
	return projects, err
}

func (s *Store) GetProject(ctx context.Context, projectID string) (*model.Project, error) {
	var project model.Project
	err := s.read.WithContext(ctx).
		Preload("Owner").
		Preload("SandboxProviderInstances").
		First(&project, "id = ?", projectID).Error
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &project, nil
}
