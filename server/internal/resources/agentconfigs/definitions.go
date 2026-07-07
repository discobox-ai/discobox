package agentconfigs

import (
	"context"
	"net/http"

	"github.com/obot-platform/discobox/server/internal/agentdefs"
	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/model"
)

// Definitions returns the built-in agent-config templates, owned by the harness
// packages and exposed through agentdefs.
func Definitions() []model.AgentConfigDefinition {
	return agentdefs.Definitions()
}

func DefinitionByID(definitionID string) (*model.AgentConfigDefinition, bool) {
	return agentdefs.DefinitionByID(definitionID)
}

func (s *Service) ListAgentConfigDefinitions(context.Context) ([]model.AgentConfigDefinition, error) {
	return agentdefs.Definitions(), nil
}

func (s *Service) GetAgentConfigDefinition(_ context.Context, definitionID string) (*model.AgentConfigDefinition, error) {
	definition, ok := agentdefs.DefinitionByID(definitionID)
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "agent config definition not found")
	}
	return definition, nil
}
