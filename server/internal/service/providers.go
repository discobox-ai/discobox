package service

import (
	"context"

	"github.com/obot-platform/discobox/server/internal/resources/providers"
)

func (s *Service) EnsureExistingSandboxProviderInstances(ctx context.Context) error {
	return s.providers().EnsureExistingSandboxProviderInstances(ctx)
}

func (s *Service) ReconcileProviderJob(ctx context.Context, projectID, providerID, jobID string) error {
	return s.providers().ReconcileProviderJob(ctx, projectID, providerID, jobID)
}

func (s *Service) enqueueProviderWorkers(ctx context.Context, projectID, providerID string) error {
	return s.providers().EnqueueProviderWorkers(ctx, projectID, providerID)
}

func (s *Service) providers() *providers.Service {
	if s.providerService == nil {
		providerService := providers.NewService(s.store, nil, s.workerManager)
		providerService.SetJobManager(s.jobManager)
		return providerService
	}
	return s.providerService
}
