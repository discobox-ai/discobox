package service

import (
	"context"
	"encoding/json"

	"github.com/danielgtaylor/huma/v2"
	"github.com/obot-platform/discobox/model"
)

var agentConfigDefinitions = []model.AgentConfigDefinition{
	{
		ID:             "codex",
		Name:           "Codex",
		Description:    "OpenAI Codex coding agent.",
		InstallCommand: "npm install -g @openai/codex",
		RunCommand:     "codex exec",
		Capabilities:   json.RawMessage(`{"tools":["shell","edit"]}`),
	},
	{
		ID:             "claude-code",
		Name:           "Claude Code",
		Description:    "Anthropic Claude Code coding agent.",
		InstallCommand: "npm install -g @anthropic-ai/claude-code",
		RunCommand:     "claude",
		Capabilities:   json.RawMessage(`{"tools":["shell","edit"]}`),
	},
}

func (s *Service) ListAgentConfigDefinitions(context.Context) ([]model.AgentConfigDefinition, error) {
	return cloneAgentConfigDefinitions(agentConfigDefinitions), nil
}

func (s *Service) GetAgentConfigDefinition(_ context.Context, definitionID string) (*model.AgentConfigDefinition, error) {
	definition, ok := agentConfigDefinitionByID(definitionID)
	if !ok {
		return nil, huma.Error404NotFound("agent config definition not found")
	}
	return definition, nil
}

func agentConfigDefinitionByID(definitionID string) (*model.AgentConfigDefinition, bool) {
	for _, definition := range agentConfigDefinitions {
		if definition.ID == definitionID {
			definition.Capabilities = cloneRawMessage(definition.Capabilities)
			return &definition, true
		}
	}
	return nil, false
}

func cloneAgentConfigDefinitions(definitions []model.AgentConfigDefinition) []model.AgentConfigDefinition {
	out := make([]model.AgentConfigDefinition, len(definitions))
	for i, definition := range definitions {
		definition.Capabilities = cloneRawMessage(definition.Capabilities)
		out[i] = definition
	}
	return out
}

func cloneRawMessage(in json.RawMessage) json.RawMessage {
	if in == nil {
		return nil
	}
	out := make(json.RawMessage, len(in))
	copy(out, in)
	return out
}
