package store

import (
	"context"

	"github.com/discobox-ai/discobox/server/internal/model"
)

func (s *Store) GetServerState(ctx context.Context, key string) (*model.ServerState, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var state model.ServerState
	if err := read.First(&state, "key = ?", key).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return &state, nil
}

func (s *Store) CreateServerState(ctx context.Context, state *model.ServerState) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Create(state).Error
}

func (s *Store) DeleteServerState(ctx context.Context, key string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	result := write.Delete(&model.ServerState{}, "key = ?", key)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
