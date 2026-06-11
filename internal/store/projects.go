package store

import (
	"context"

	"github.com/obot-platform/disco2/internal/model"
)

func (s *Store) UpsertTenant(ctx context.Context, tenant *model.Tenant) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Save(tenant).Error
}

func (s *Store) UpsertProject(ctx context.Context, project *model.Project) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Save(project).Error
}

func (s *Store) ListProjects(ctx context.Context) ([]model.Project, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var projects []model.Project
	err = read.
		Order("created_at ASC").
		Find(&projects).Error
	return projects, err
}

func (s *Store) GetProject(ctx context.Context, projectID string) (*model.Project, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var project model.Project
	err = read.
		Preload("SandboxProviderInstances").
		First(&project, "id = ?", projectID).Error
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &project, nil
}
