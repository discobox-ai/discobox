package service

import (
	"context"

	"github.com/discobox-ai/discobox/server/internal/resources/providers"
)

func (s *Service) EnsureExistingSandboxProviderInstances(ctx context.Context) error {
	return s.providers().EnsureExistingSandboxProviderInstances(ctx)
}

func (s *Service) enqueueProviderPools(ctx context.Context, projectID, providerID string) error {
	return s.providers().EnqueueProviderPools(ctx, projectID, providerID)
}

func (s *Service) providers() *providers.Service {
	if s.providerService == nil {
		return providers.NewService(s.store, nil, s.poolControlPlane)
	}
	return s.providerService
}
