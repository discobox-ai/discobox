// Package store contains database access methods.
package store

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/obot-platform/disco2/internal/model"
	"github.com/obot-platform/disco2/internal/secrets"
)

var (
	ErrNotFound           = errors.New("record not found")
	ErrGenerationConflict = errors.New("generation conflict")
)

// Store uses separate GORM handles for writes and reads.
type Store struct {
	write             *gorm.DB
	read              *gorm.DB
	publisher         EventPublisher
	afterCommitEvents *[]model.ProjectEvent
	sealer            secrets.Sealer
}

type EventPublisher interface {
	PublishProjectEvent(ctx context.Context, event model.ProjectEvent)
}

func New(write, read *gorm.DB, publisher ...EventPublisher) *Store {
	if read == nil {
		read = write
	}
	var p EventPublisher
	if len(publisher) > 0 {
		p = publisher[0]
	}
	return &Store{write: write, read: read, publisher: p}
}

func (s *Store) WithSealer(sealer secrets.Sealer) *Store {
	s.sealer = sealer
	return s
}

func mapNotFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}
