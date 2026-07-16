package harnessconfigs

import (
	"context"
	"net/http"

	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/harnessdefs"
	"github.com/obot-platform/discobox/server/internal/model"
)

// Definitions returns the built-in harness-config templates with baked-in
// images, owned by the harness packages and exposed through harnessdefs.
func Definitions() []model.HarnessDefinition {
	return harnessdefs.Definitions()
}

func DefinitionByID(definitionID string) (*model.HarnessDefinition, bool) {
	return harnessdefs.DefinitionByID(definitionID)
}

// definitions returns the built-in harness-config templates with per-definition
// image overrides applied, so definition-backed configs resolve dev images.
func (s *Service) definitions() []model.HarnessDefinition {
	return harnessdefs.DefinitionsWithImages(s.harnessImages)
}

// definitionByID returns the built-in definition with image overrides applied.
func (s *Service) definitionByID(definitionID string) (*model.HarnessDefinition, bool) {
	return harnessdefs.DefinitionByIDWithImages(definitionID, s.harnessImages)
}

func (s *Service) ListHarnessDefinitions(context.Context) ([]model.HarnessDefinition, error) {
	return s.definitions(), nil
}

func (s *Service) GetHarnessDefinition(_ context.Context, definitionID string) (*model.HarnessDefinition, error) {
	definition, ok := s.definitionByID(definitionID)
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "harness config definition not found")
	}
	return definition, nil
}
