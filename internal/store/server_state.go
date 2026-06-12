package store

import (
	"context"

	"github.com/obot-platform/discobox/internal/model"
)

func (s *Store) GetServerState(ctx context.Context, key string) (*model.ServerState, error) {
	tenantID, err := s.tenantID(ctx)
	if err != nil {
		return nil, err
	}
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var state model.ServerState
	if err := read.First(&state, "tenant_id = ? AND key = ?", tenantID, key).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return &state, nil
}

func (s *Store) CreateServerState(ctx context.Context, state *model.ServerState) error {
	tenantID, err := s.tenantID(ctx)
	if err != nil {
		return err
	}
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	if state.TenantID == "" {
		state.TenantID = tenantID
	}
	return write.Create(state).Error
}

func (s *Store) DeleteServerState(ctx context.Context, key string) error {
	tenantID, err := s.tenantID(ctx)
	if err != nil {
		return err
	}
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	result := write.Delete(&model.ServerState{}, "tenant_id = ? AND key = ?", tenantID, key)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
