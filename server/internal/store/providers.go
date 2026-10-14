package store

import (
	"context"

	"github.com/discobox-ai/discobox/server/internal/model"
)

func (s *Store) ListSandboxProviderInstances(ctx context.Context, projectID string) ([]model.SandboxProviderInstance, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var providers []model.SandboxProviderInstance
	err = read.
		Where("project_id = ?", projectID).
		Order("created_at ASC").
		Find(&providers).Error
	return providers, err
}

func (s *Store) CreateSandboxProviderInstance(ctx context.Context, provider *model.SandboxProviderInstance) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Create(provider).Error
}

func (s *Store) GetSandboxProviderInstance(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.SandboxProviderInstance](read.Where("project_id = ?", projectID), "id", providerID)
}

func (s *Store) UpdateSandboxProviderInstance(ctx context.Context, provider *model.SandboxProviderInstance) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Save(provider).Error
}

func (s *Store) DeleteSandboxProviderInstance(ctx context.Context, projectID, providerID string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	provider, err := firstByID[model.SandboxProviderInstance](write.Where("project_id = ?", projectID), "id", providerID)
	if err != nil {
		return err
	}
	return write.Delete(provider).Error
}
