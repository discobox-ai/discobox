package agentconfigs

import (
	"context"
	"net/http"

	"github.com/obot-platform/discobox/server/internal/apperrors"

	"github.com/obot-platform/discobox/server/internal/model"
)

var agentConfigDefinitions = []model.AgentConfigDefinition{
	{
		ID:             "codex",
		Name:           "Codex",
		Description:    "OpenAI Codex coding agent.",
		InstallCommand: "npm install -g @openai/codex",
		RunCommand:     "codex",
	},
	{
		ID:             "claude-code",
		Name:           "Claude Code",
		Description:    "Anthropic Claude Code coding agent.",
		InstallCommand: "npm install -g @anthropic-ai/claude-code",
		RunCommand:     "claude",
	},
}

func Definitions() []model.AgentConfigDefinition {
	return cloneAgentConfigDefinitions(agentConfigDefinitions)
}

func DefinitionByID(definitionID string) (*model.AgentConfigDefinition, bool) {
	return agentConfigDefinitionByID(definitionID)
}

func (s *Service) ListAgentConfigDefinitions(context.Context) ([]model.AgentConfigDefinition, error) {
	return Definitions(), nil
}

func (s *Service) GetAgentConfigDefinition(_ context.Context, definitionID string) (*model.AgentConfigDefinition, error) {
	definition, ok := agentConfigDefinitionByID(definitionID)
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "agent config definition not found")
	}
	return definition, nil
}

func agentConfigDefinitionByID(definitionID string) (*model.AgentConfigDefinition, bool) {
	for _, definition := range agentConfigDefinitions {
		if definition.ID == definitionID {
			return &definition, true
		}
	}
	return nil, false
}

func cloneAgentConfigDefinitions(definitions []model.AgentConfigDefinition) []model.AgentConfigDefinition {
	out := make([]model.AgentConfigDefinition, len(definitions))
	copy(out, definitions)
	return out
}
