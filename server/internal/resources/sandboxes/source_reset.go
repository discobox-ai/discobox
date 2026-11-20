package sandboxes

import (
	"context"
	"errors"
	"math"
	"net/http"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func (s *Service) GetSandboxSourceReset(ctx context.Context, projectID, sandboxID, slug string) (model.SandboxSourceReset, error) {
	if err := authorizeSourceReset(ctx, s.store, projectID, sandboxID, slug, false); err != nil {
		return model.SandboxSourceReset{}, err
	}
	return s.store.GetSourceReset(ctx, sandboxID, slug)
}

func (s *Service) PostSandboxSourceReset(ctx context.Context, projectID, sandboxID, slug string, expected int64, commit string) (model.SandboxSourceReset, error) {
	if expected < 0 || expected == math.MaxInt64 {
		return model.SandboxSourceReset{}, apperrors.NewStatusError(http.StatusUnprocessableEntity, "invalid expected reset revision")
	}
	if err := (sandboxconfig.SourceReset{Revision: expected + 1, Commit: commit}).Validate(); err != nil {
		return model.SandboxSourceReset{}, apperrors.NewStatusError(http.StatusUnprocessableEntity, err.Error())
	}
	result, err := s.store.SetSourceReset(ctx, projectID, sandboxID, slug, commit, auth.ActorFromContext(ctx), expected, func(tx *store.Store) error {
		return authorizeSourceReset(ctx, tx, projectID, sandboxID, slug, true)
	})
	if errors.Is(err, store.ErrSourceResetRevision) {
		err = apperrors.NewStatusError(http.StatusConflict, err.Error())
	} else if errors.Is(err, store.ErrNotFound) {
		err = apperrors.NotFound(err, "sandbox not found")
	}
	return result, err
}

func authorizeSourceReset(ctx context.Context, db *store.Store, projectID, sandboxID, slug string, write bool) error {
	if err := authorizeSandboxManagement(ctx, db, projectID, sandboxID, write); err != nil {
		return err
	}
	box, err := db.GetSandbox(ctx, projectID, sandboxID, store.SandboxFromPrimary())
	if err != nil {
		return apperrors.NotFound(err, "sandbox not found")
	}
	for _, entry := range clientDeliveredSources(box) {
		if entry.source.Slug != nil && *entry.source.Slug == slug && !entry.source.IsSkills() {
			if write && box.SourceDeliveredAt == nil && sandboxHasNeverRun(box.State) {
				return apperrors.NewStatusError(http.StatusConflict, "the source has not been delivered")
			}
			return nil
		}
	}
	return apperrors.NewStatusError(http.StatusNotFound, "push-delivered work source not found")
}
