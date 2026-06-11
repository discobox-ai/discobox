// Package store contains database access methods.
package store

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/obot-platform/disco2/internal/model"
	"github.com/obot-platform/disco2/internal/secrets"
	"github.com/obot-platform/disco2/internal/tenantctx"
)

var (
	ErrNotFound           = errors.New("record not found")
	ErrGenerationConflict = errors.New("generation conflict")
)

// TenantDBResolver resolves tenant-scoped database handles.
type TenantDBResolver interface {
	ResolveTenantDB(ctx context.Context, tenantID string) (write, read *gorm.DB, err error)
}

// Store resolves tenant-scoped GORM handles from context for each operation.
type Store struct {
	resolver TenantDBResolver
	txWrite  *gorm.DB
	txRead   *gorm.DB

	publisher         EventPublisher
	afterCommitEvents *[]model.ProjectEvent
	sealer            secrets.Sealer
	defaultTenantID   string
}

type EventPublisher interface {
	PublishProjectEvent(ctx context.Context, event model.ProjectEvent)
}

type Option func(*Store)

func WithPublisher(publisher EventPublisher) Option {
	return func(s *Store) {
		s.publisher = publisher
	}
}

func WithSealer(sealer secrets.Sealer) Option {
	return func(s *Store) {
		s.sealer = sealer
	}
}

// WithDefaultTenantID configures an explicit fallback tenant for single-tenant
// test wiring where requests cannot carry middleware-provided context.
func WithDefaultTenantID(tenantID string) Option {
	return func(s *Store) {
		s.defaultTenantID = tenantID
	}
}

func New(resolver TenantDBResolver, options ...Option) *Store {
	s := &Store{resolver: resolver}
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

func (s *Store) resolve(ctx context.Context) (*gorm.DB, *gorm.DB, error) {
	if s == nil || s.resolver == nil {
		return nil, nil, errors.New("tenant database resolver is required")
	}
	tenantID, err := tenantctx.TenantID(ctx)
	if err != nil {
		if s.defaultTenantID == "" {
			return nil, nil, err
		}
		tenantID = s.defaultTenantID
	}
	return s.resolver.ResolveTenantDB(ctx, tenantID)
}

func (s *Store) tenantID(ctx context.Context) (string, error) {
	tenantID, err := tenantctx.TenantID(ctx)
	if err != nil {
		if s.defaultTenantID == "" {
			return "", err
		}
		return s.defaultTenantID, nil
	}
	return tenantID, nil
}

func (s *Store) withTx(write, read *gorm.DB, events *[]model.ProjectEvent) *Store {
	if read == nil {
		read = write
	}
	return &Store{
		resolver:          s.resolver,
		txWrite:           write,
		txRead:            read,
		publisher:         s.publisher,
		afterCommitEvents: events,
		sealer:            s.sealer,
		defaultTenantID:   s.defaultTenantID,
	}
}

func mapNotFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}
