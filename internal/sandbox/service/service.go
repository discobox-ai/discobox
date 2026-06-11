package sandboxservice

import (
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"github.com/obot-platform/disco2/internal/sandbox"
	"github.com/obot-platform/disco2/internal/sandboxauth"
	"github.com/obot-platform/disco2/internal/store"
)

// Service owns sandbox API behavior, orchestration, provider catalog state, and
// sandbox auth dependencies.
type Service struct {
	store            *store.Store
	sandboxes        *sandbox.SandboxOrchestrator
	sandboxProviders *sandbox.ProviderManager
	sandboxAuth      *sandboxauth.Manager
	defaultUserID    string
}

func NewService(store *store.Store, orchestrator *sandbox.SandboxOrchestrator, manager *sandbox.ProviderManager, defaultUserID string) *Service {
	return &Service{
		store:            store,
		sandboxes:        orchestrator,
		sandboxProviders: manager,
		defaultUserID:    defaultUserID,
	}
}

func (s *Service) SetSandboxAuthManager(manager *sandboxauth.Manager) {
	s.sandboxAuth = manager
}

func mapAPIError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return huma.Error404NotFound(notFoundMessage)
	}
	return err
}
