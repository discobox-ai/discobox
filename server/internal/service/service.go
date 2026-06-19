// Package service contains application services.
package service

import (
	"context"
	"errors"
	"net/http"

	"github.com/obot-platform/discobox/apperrors"

	"github.com/obot-platform/discobox/orchestration"
	providersandbox "github.com/obot-platform/discobox/providers/sandbox/provider"
	sandbox "github.com/obot-platform/discobox/sandboxprovider"
	"github.com/obot-platform/discobox/server/internal/events"
	"github.com/obot-platform/discobox/server/internal/sandbox/jobs"
	sandboxservice "github.com/obot-platform/discobox/server/internal/sandbox/service"
	"github.com/obot-platform/discobox/server/internal/sandboxauth"
	"github.com/obot-platform/discobox/server/internal/store"
)

const (
	DefaultUserID    = "00000000000000000000000001"
	DefaultProjectID = "00000000000000000000000002"
)

// Service implements the API service interfaces using the database store.
type Service struct {
	*sandboxservice.Service
	store             *store.Store
	broker            *events.Broker
	workerStore       any
	workerSubmitter   *jobs.WorkerSubmitter
	providerSubmitter *jobs.ProviderSubmitter
}

func New(store *store.Store, queueConfig orchestration.QueueConfig, notifyNewJob func(context.Context), broker ...*events.Broker) *Service {
	var b *events.Broker
	if len(broker) > 0 {
		b = broker[0]
	}
	manager := sandbox.NewProviderManager()
	sandboxSubmitter := jobs.NewSandboxSubmitter(store, queueConfig, notifyNewJob)
	workerSubmitter := jobs.NewWorkerSubmitter(store, queueConfig, notifyNewJob)
	providerSubmitter := jobs.NewProviderSubmitter(store, queueConfig, notifyNewJob)
	workerStore := newWorkerStore(store, workerSubmitter)
	providersandbox.RegisterBuiltInSandboxProviderFactories(manager, workerStore)
	return &Service{
		Service:           sandboxservice.NewService(store, sandboxSubmitter, manager, DefaultUserID, workerStore),
		store:             store,
		broker:            b,
		workerStore:       workerStore,
		workerSubmitter:   workerSubmitter,
		providerSubmitter: providerSubmitter,
	}
}

func (s *Service) SetSandboxAuthManager(manager *sandboxauth.Manager) {
	s.Service.SetSandboxAuthManager(manager)
}

func (s *Service) ProviderSubmitter() *jobs.ProviderSubmitter {
	if s == nil {
		return nil
	}
	return s.providerSubmitter
}

func apiError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return apperrors.NewStatusError(http.StatusNotFound, notFoundMessage)
	}
	return err
}
