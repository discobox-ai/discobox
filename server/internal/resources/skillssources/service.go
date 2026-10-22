// Package skillssources implements a project's skills sources (ADR
// 26-10-14-894 §1): the skills repositories every discobox of the project is
// created with, which the creating client copies into its create request.
package skillssources

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

type Service struct {
	store *store.Store
}

func NewService(store *store.Store) *Service {
	return &Service{store: store}
}

func (s *Service) ListProjectSkillsSources(ctx context.Context, projectID string) ([]model.ProjectSkillsSource, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	return s.store.ListProjectSkillsSources(ctx, projectID)
}

func (s *Service) CreateProjectSkillsSource(ctx context.Context, projectID string, input services.CreateProjectSkillsSourceBody) (*model.ProjectSkillsSource, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	source := &model.ProjectSkillsSource{
		ProjectID: projectID,
		URL:       strings.TrimSpace(input.URL),
		Ref:       strings.TrimSpace(input.Ref.Or("")),
		Directory: strings.TrimSpace(input.Directory.Or("")),
	}
	if source.Directory == "" {
		source.Directory = sandboxconfig.RootSkillsDirectory
	}
	if err := validate(source); err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	if principal, ok := auth.PrincipalFromContext(ctx); ok {
		source.CreatedBy = principal.UserID
	}
	if err := s.store.CreateProjectSkillsSource(ctx, source); err != nil {
		if errors.Is(err, store.ErrSkillsSourceConflict) {
			return nil, apperrors.NewStatusError(http.StatusConflict, err.Error())
		}
		return nil, err
	}
	return s.store.GetProjectSkillsSource(ctx, projectID, source.ID)
}

func (s *Service) DeleteProjectSkillsSource(ctx context.Context, projectID, sourceID string) error {
	if err := s.store.DeleteProjectSkillsSource(ctx, projectID, sourceID); err != nil {
		return apperrors.NotFound(err, "skills source not found")
	}
	return nil
}

// validate refuses an entry a creating client could not, or must not, add as
// the --skills URL[@REF][//PATH] it stands for (ADR 26-10-14-894 §1). The rule
// is sandboxconfig's, so the clients reading the list hold it too.
func validate(source *model.ProjectSkillsSource) error {
	if err := sandboxconfig.ValidateSkillsRepository(source.URL, source.Ref); err != nil {
		return err
	}
	return sandboxconfig.ValidateSkillsDirectory(source.Directory)
}
