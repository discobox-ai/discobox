package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/server/internal/api"
)

func (s *Service) ListAgentConfigs(ctx context.Context, projectID string) ([]model.AgentConfig, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	return s.store.ListAgentConfigs(ctx, projectID)
}

func (s *Service) CreateAgentConfig(ctx context.Context, projectID string, input api.CreateAgentConfigBody) (*model.AgentConfig, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	var definition *model.AgentConfigDefinition
	if input.DefinitionID != nil {
		var ok bool
		definition, ok = agentConfigDefinitionByID(*input.DefinitionID)
		if !ok {
			return nil, huma.Error404NotFound("agent config definition not found")
		}
	}
	name := strings.TrimSpace(input.Name)
	if name == "" && definition != nil {
		name = definition.Name
	}
	if name == "" {
		return nil, apiError(fmt.Errorf("agent config name is required"), "")
	}
	installCommand := input.InstallCommand
	if installCommand == "" && definition != nil {
		installCommand = definition.InstallCommand
	}
	runCommand := input.RunCommand
	if strings.TrimSpace(runCommand) == "" && definition != nil {
		runCommand = definition.RunCommand
	}
	if strings.TrimSpace(runCommand) == "" {
		return nil, apiError(fmt.Errorf("agent config run command is required"), "")
	}
	capabilities := input.Capabilities
	if capabilities == nil && definition != nil {
		capabilities = cloneRawMessage(definition.Capabilities)
	}
	config := &model.AgentConfig{
		ProjectID:      projectID,
		Name:           name,
		InstallCommand: installCommand,
		RunCommand:     runCommand,
		Capabilities:   capabilities,
	}
	if err := s.store.CreateAgentConfig(ctx, config); err != nil {
		return nil, err
	}
	return s.store.GetAgentConfig(ctx, projectID, config.ID)
}

func (s *Service) GetAgentConfig(ctx context.Context, projectID, configID string) (*model.AgentConfig, error) {
	config, err := s.store.GetAgentConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apiError(err, "agent config not found")
	}
	return config, nil
}

func (s *Service) UpdateAgentConfig(ctx context.Context, projectID, configID string, input api.UpdateAgentConfigBody) (*model.AgentConfig, error) {
	config, err := s.store.GetAgentConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apiError(err, "agent config not found")
	}
	if input.Name != nil {
		name := strings.TrimSpace(*input.Name)
		if name == "" {
			return nil, apiError(fmt.Errorf("agent config name is required"), "")
		}
		config.Name = name
	}
	if input.InstallCommand != nil {
		config.InstallCommand = *input.InstallCommand
	}
	if input.RunCommand != nil {
		if strings.TrimSpace(*input.RunCommand) == "" {
			return nil, apiError(fmt.Errorf("agent config run command is required"), "")
		}
		config.RunCommand = *input.RunCommand
	}
	if input.Capabilities != nil {
		config.Capabilities = input.Capabilities
	}
	if err := s.store.UpdateAgentConfig(ctx, config); err != nil {
		return nil, err
	}
	return s.store.GetAgentConfig(ctx, projectID, configID)
}

func (s *Service) DeleteAgentConfig(ctx context.Context, projectID, configID string) error {
	return apiError(s.store.DeleteAgentConfig(ctx, projectID, configID), "agent config not found")
}
