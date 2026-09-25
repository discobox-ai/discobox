package secrets

import (
	"context"
	"errors"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// ListSecretRefreshEvents reads the refresh audit trail (ADR 26-09-25-122 §6):
// every ask for a new value and how it was answered, with the secret named
// while it still exists. Nothing here is a value.
func (s *Service) ListSecretRefreshEvents(ctx context.Context, projectID string, filter store.SecretRefreshFilter) ([]model.SecretRefreshEvent, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	events, err := s.store.ListSecretRefreshEvents(ctx, projectID, filter)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for i := range events {
		id := events[i].SecretID
		name, ok := names[id]
		if !ok {
			secret, err := s.store.GetSecret(ctx, projectID, id)
			switch {
			case err == nil:
				name = secret.Name
			case !errors.Is(err, store.ErrNotFound):
				return nil, err
			}
			names[id] = name
		}
		events[i].SecretName = name
	}
	return events, nil
}
