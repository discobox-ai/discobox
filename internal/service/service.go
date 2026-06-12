// Package service contains application services.
package service

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"github.com/obot-platform/discobox/internal/events"
	"github.com/obot-platform/discobox/internal/sandbox"
	"github.com/obot-platform/discobox/internal/sandbox/jobs"
	sandboxprovider "github.com/obot-platform/discobox/internal/sandbox/provider"
	sandboxservice "github.com/obot-platform/discobox/internal/sandbox/service"
	"github.com/obot-platform/discobox/internal/sandboxauth"
	"github.com/obot-platform/discobox/internal/store"
	"github.com/obot-platform/discobox/orchestration"
)

const (
	DefaultTenantID  = "00000000-0000-0000-0000-000000000000"
	DefaultUserID    = "00000000-0000-0000-0000-000000000001"
	DefaultProjectID = "00000000-0000-0000-0000-000000000002"
)

// Service implements the API service interfaces using the database store.
type Service struct {
	*sandboxservice.Service
	store       *store.Store
	broker      *events.Broker
	workerStore any
}

func New(store *store.Store, queueConfig orchestration.QueueConfig, notifyNewJob func(context.Context), broker ...*events.Broker) *Service {
	var b *events.Broker
	if len(broker) > 0 {
		b = broker[0]
	}
	manager := sandbox.NewProviderManager()
	sandboxSubmitter := jobs.NewSandboxSubmitter(store, queueConfig, notifyNewJob)
	workerSubmitter := jobs.NewWorkerSubmitter(store, queueConfig, notifyNewJob)
	workerStore := newWorkerStore(store, workerSubmitter)
	sandboxprovider.RegisterBuiltInSandboxProviderFactories(manager, workerStore)
	return &Service{
		Service:     sandboxservice.NewService(store, sandboxSubmitter, manager, DefaultUserID, workerStore),
		store:       store,
		broker:      b,
		workerStore: workerStore,
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
