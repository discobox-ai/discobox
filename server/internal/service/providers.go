package service

import (
	"context"

	"github.com/obot-platform/discobox/server/internal/resources/providers"
)

func (s *Service) EnsureExistingSandboxProviderInstances(ctx context.Context) error {
	return s.providers().EnsureExistingSandboxProviderInstances(ctx)
}

func (s *Service) enqueueProviderWorkers(ctx context.Context, projectID, providerID string) error {
	return s.providers().EnqueueProviderWorkers(ctx, projectID, providerID)
}

func (s *Service) providers() *providers.Service {
	if s.providerService == nil {
		return providers.NewService(s.store, nil, s.workerManager, s.jobManager)
	}
	return s.providerService
}
