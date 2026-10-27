// Package store contains database access methods.
package store

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/secrets"
)

var (
	ErrNotFound           = apperrors.ErrNotFound
	ErrGenerationConflict = apperrors.ErrGenerationConflict
	// ErrInUse indicates a resource cannot be deleted because another live
	// resource still references it.
	ErrInUse = errors.New("resource is in use")
)

// Store owns GORM handles for application persistence.
type Store struct {
	write   *gorm.DB
	read    *gorm.DB
	txWrite *gorm.DB
	txRead  *gorm.DB

	sealer secrets.Sealer
}

type Option func(*Store)

func WithSealer(sealer secrets.Sealer) Option {
	return func(s *Store) {
		s.sealer = sealer
	}
}

func New(write, read *gorm.DB, options ...Option) *Store {
	if read == nil {
		read = write
	}
	s := &Store{write: write, read: read}
	for _, option := range options {
		if option != nil {
			option(s)
		}
	}
	return s
}

func (s *Store) getRead(ctx context.Context) (*gorm.DB, error) {
	if s.txRead != nil {
		return s.txRead.WithContext(ctx), nil
	}
	write, read, err := s.resolve(ctx)
	if err != nil {
		return nil, err
	}
	if read == nil {
		read = write
	}
	return read.WithContext(ctx), nil
}

func (s *Store) getWrite(ctx context.Context) (*gorm.DB, error) {
	if s.txWrite != nil {
		return s.txWrite.WithContext(ctx), nil
	}
	write, _, err := s.resolve(ctx)
	if err != nil {
		return nil, err
	}
	return write.WithContext(ctx), nil
}

func (s *Store) resolve(_ context.Context) (*gorm.DB, *gorm.DB, error) {
	if s == nil || s.write == nil {
		return nil, nil, errors.New("database write handle is required")
	}
	read := s.read
	if read == nil {
		read = s.write
	}
	return s.write, read, nil
}

func (s *Store) withTx(write, read *gorm.DB) *Store {
	if read == nil {
		read = write
	}
	return &Store{
		write:   s.write,
		read:    s.read,
		txWrite: write,
		txRead:  read,
		sealer:  s.sealer,
	}
}

func mapNotFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}
