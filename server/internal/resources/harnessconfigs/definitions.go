package harnessconfigs

import (
	"context"
	"net/http"

	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/harnessdefs"
	"github.com/obot-platform/discobox/server/internal/model"
)

// Definitions returns the built-in harness-config templates, owned by the harness
// packages and exposed through harnessdefs.
func Definitions() []model.HarnessDefinition {
	return harnessdefs.Definitions()
}

func DefinitionByID(definitionID string) (*model.HarnessDefinition, bool) {
	return harnessdefs.DefinitionByID(definitionID)
}

func (s *Service) ListHarnessDefinitions(context.Context) ([]model.HarnessDefinition, error) {
	return harnessdefs.Definitions(), nil
}

func (s *Service) GetHarnessDefinition(_ context.Context, definitionID string) (*model.HarnessDefinition, error) {
	definition, ok := harnessdefs.DefinitionByID(definitionID)
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "harness config definition not found")
	}
	return definition, nil
}
