package store

import (
	"context"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/server/internal/model"
)

type SandboxProviderInstanceID struct {
	ProjectID  string
	ProviderID string
}

type SandboxProviderInstanceStore struct {
	*Store
	reload *Store
}

func (s *Store) SandboxProviderInstances() *SandboxProviderInstanceStore {
	return &SandboxProviderInstanceStore{Store: s, reload: s}
}

func (s *SandboxProviderInstanceStore) Transaction(ctx context.Context, fn func(context.Context, *SandboxProviderInstanceStore) error) error {
	return s.Store.Transaction(ctx, func(txStore *Store, _ *gorm.DB) error {
		return fn(ctx, &SandboxProviderInstanceStore{Store: txStore, reload: s.reload})
	})
}

func (s *SandboxProviderInstanceStore) Get(ctx context.Context, id SandboxProviderInstanceID) (*model.SandboxProviderInstance, error) {
	return s.GetSandboxProviderInstance(ctx, id.ProjectID, id.ProviderID)
}

func (s *SandboxProviderInstanceStore) Create(ctx context.Context, provider *model.SandboxProviderInstance) error {
	return s.CreateSandboxProviderInstance(ctx, provider)
}

func (s *SandboxProviderInstanceStore) Update(ctx context.Context, provider *model.SandboxProviderInstance) error {
	return s.UpdateSandboxProviderInstance(ctx, provider)
}

func (s *SandboxProviderInstanceStore) ID(provider *model.SandboxProviderInstance) SandboxProviderInstanceID {
	return SandboxProviderInstanceID{ProjectID: provider.ProjectID, ProviderID: provider.ID}
}

func (s *SandboxProviderInstanceStore) Reload(ctx context.Context, id SandboxProviderInstanceID) (*model.SandboxProviderInstance, error) {
	return s.reload.GetSandboxProviderInstance(ctx, id.ProjectID, id.ProviderID)
}

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
