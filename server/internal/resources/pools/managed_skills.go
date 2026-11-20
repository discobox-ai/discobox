package pools

import (
	"context"
	"net/http"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
)

func (s *Service) GetPoolSandboxRuntimeIntent(ctx context.Context, poolID, sandboxID string, known int64) (*sandboxconfig.PublishedSandboxRuntime, error) {
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypePool || principal.PoolID != poolID {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "hosting pool access required")
	}
	current, err := s.store.GetPoolManagedSkills(ctx, poolID, sandboxID, known)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	resets, err := s.store.HostedSourceResets(ctx, poolID, sandboxID)
	if err != nil {
		return nil, err
	}
	if current == nil && len(resets) == 0 {
		return nil, nil
	}
	response := &sandboxconfig.PublishedSandboxRuntime{SourceResets: resets}
	if current != nil {
		response.ManagedSkills = *current
	}
	return response, nil
}
