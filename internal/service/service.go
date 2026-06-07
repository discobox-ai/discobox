// Package service contains application services.
package service

import (
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"github.com/obot-platform/disco2/internal/events"
	"github.com/obot-platform/disco2/internal/orchestration"
	"github.com/obot-platform/disco2/internal/sandbox"
	"github.com/obot-platform/disco2/internal/sandboxauth"
	"github.com/obot-platform/disco2/internal/store"
)

const (
	DefaultUserID    = "00000000-0000-0000-0000-000000000001"
	DefaultProjectID = "00000000-0000-0000-0000-000000000002"
)

// Service implements the API service interfaces using the database store.
type Service struct {
	store            *store.Store
	broker           *events.Broker
	sandboxes        *SandboxOrchestrator
	sandboxProviders *sandbox.ProviderManager
	sandboxAuth      *sandboxauth.Manager
}

func New(store *store.Store, orchestrator *orchestration.Orchestrator, broker ...*events.Broker) *Service {
	if orchestrator == nil {
		panic("service orchestrator is required")
	}
	var b *events.Broker
	if len(broker) > 0 {
		b = broker[0]
	}
	manager := sandbox.NewProviderManager()
	return &Service{
		store:            store,
		broker:           b,
		sandboxes:        NewSandboxOrchestrator(store, orchestrator),
		sandboxProviders: manager,
	}
}

func (s *Service) SetSandboxAuthManager(manager *sandboxauth.Manager) {
	s.sandboxAuth = manager
}

func apiError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return huma.Error404NotFound(notFoundMessage)
	}
	return err
}
