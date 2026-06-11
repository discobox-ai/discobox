// Package service contains application services.
package service

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"github.com/obot-platform/disco2/internal/events"
	"github.com/obot-platform/disco2/internal/sandbox"
	"github.com/obot-platform/disco2/internal/sandbox/jobs"
	sandboxprovider "github.com/obot-platform/disco2/internal/sandbox/provider"
	sandboxservice "github.com/obot-platform/disco2/internal/sandbox/service"
	"github.com/obot-platform/disco2/internal/sandboxauth"
	"github.com/obot-platform/disco2/internal/store"
	"github.com/obot-platform/disco2/orchestration"
)

const (
	DefaultTenantID  = "00000000-0000-0000-0000-000000000000"
	DefaultUserID    = "00000000-0000-0000-0000-000000000001"
	DefaultProjectID = "00000000-0000-0000-0000-000000000002"
)

// Service implements the API service interfaces using the database store.
type Service struct {
	*sandboxservice.Service
	store  *store.Store
	broker *events.Broker
}

func New(store *store.Store, queueConfig orchestration.QueueConfig, notifyNewJob func(context.Context), broker ...*events.Broker) *Service {
	var b *events.Broker
	if len(broker) > 0 {
		b = broker[0]
	}
	manager := sandbox.NewProviderManager()
	sandboxprovider.RegisterBuiltInSandboxProviderFactories(manager, store)
	sandboxSubmitter := jobs.NewSandboxSubmitter(store, queueConfig, notifyNewJob)
	return &Service{
		Service: sandboxservice.NewService(store, sandboxSubmitter, manager, DefaultUserID),
		store:   store,
		broker:  b,
	}
}

func (s *Service) SetSandboxAuthManager(manager *sandboxauth.Manager) {
	s.Service.SetSandboxAuthManager(manager)
}

func apiError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return huma.Error404NotFound(notFoundMessage)
	}
	return err
}
