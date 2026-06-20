package projects

import (
	"context"
	"errors"
	"net/http"

	"github.com/obot-platform/discobox/apperrors"
	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/server/internal/auth"
	"github.com/obot-platform/discobox/server/internal/store"
)

type Service struct {
	store *store.Store
}

func NewService(store *store.Store) *Service {
	return &Service{store: store}
}

func (s *Service) ListProjects(ctx context.Context) ([]model.Project, error) {
	if userID, err := auth.UserID(ctx); err == nil {
		return s.store.ListProjectsForUser(ctx, userID)
	}
	return s.store.ListProjects(ctx)
}

func (s *Service) GetProject(ctx context.Context, projectID string) (*model.Project, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apiError(err, "project not found")
	}
	return project, nil
}

func apiError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return apperrors.NewStatusError(http.StatusNotFound, notFoundMessage)
	}
	return err
}
