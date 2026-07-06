package agentconfigs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/server/internal/apperrors"

	"github.com/obot-platform/discobox/server/internal/model"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
)

type Service struct {
	store *store.Store
}

func NewService(store *store.Store) *Service {
	return &Service{store: store}
}

func (s *Service) ListAgentConfigs(ctx context.Context, projectID string) ([]model.AgentConfig, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	return s.store.ListAgentConfigs(ctx, projectID)
}

func (s *Service) CreateAgentConfig(ctx context.Context, projectID string, input services.CreateAgentConfigBody) (*model.AgentConfig, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apiError(err, "project not found")
	}
	var definition *model.AgentConfigDefinition
	if definitionID, isSet := input.DefinitionId.Get(); isSet {
		var found bool
		definition, found = agentConfigDefinitionByID(definitionID)
		if !found {
			return nil, apperrors.NewStatusError(http.StatusNotFound, "agent config definition not found")
		}
	}
	name := strings.TrimSpace(input.Name.Or(""))
	if name == "" && definition != nil {
		name = definition.Name
	}
	if name == "" {
		return nil, apiError(fmt.Errorf("agent config name is required"), "")
	}
	installCommand, hasInstallCommand := input.InstallCommand.Get()
	if !hasInstallCommand && definition != nil {
		installCommand = definition.InstallCommand
	}
	runCommand, hasRunCommand := input.RunCommand.Get()
	if !hasRunCommand && definition != nil {
		runCommand = definition.RunCommand
	}
	if len(runCommand) == 0 {
		return nil, apiError(fmt.Errorf("agent config run command is required"), "")
	}
	var files []model.AgentConfigFile
	if apiFiles, ok := input.Files.Get(); ok {
		files = agentConfigFilesFromAPI(apiFiles)
	} else if definition != nil {
		files = definition.Files
	}
	config := &model.AgentConfig{
		ProjectID:      projectID,
		Name:           name,
		InstallCommand: installCommand,
		RunCommand:     runCommand,
		Files:          files,
	}
	if err := s.store.CreateAgentConfig(ctx, config); err != nil {
		return nil, err
	}
	if project.DefaultAgentConfigID == "" {
		project.DefaultAgentConfigID = config.ID
		if err := s.store.UpsertProject(ctx, project); err != nil {
			return nil, err
		}
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

func (s *Service) UpdateAgentConfig(ctx context.Context, projectID, configID string, input services.UpdateAgentConfigBody) (*model.AgentConfig, error) {
	config, err := s.store.GetAgentConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apiError(err, "agent config not found")
	}
	if nameValue, ok := input.Name.Get(); ok {
		name := strings.TrimSpace(nameValue)
		if name == "" {
			return nil, apiError(fmt.Errorf("agent config name is required"), "")
		}
		config.Name = name
	}
	if installCommand, ok := input.InstallCommand.Get(); ok {
		config.InstallCommand = installCommand
	}
	if runCommand, ok := input.RunCommand.Get(); ok {
		if len(runCommand) == 0 {
			return nil, apiError(fmt.Errorf("agent config run command is required"), "")
		}
		config.RunCommand = runCommand
	}
	if apiFiles, ok := input.Files.Get(); ok {
		config.Files = agentConfigFilesFromAPI(apiFiles)
	}
	if err := s.store.UpdateAgentConfig(ctx, config); err != nil {
		return nil, err
	}
	return s.store.GetAgentConfig(ctx, projectID, configID)
}

func agentConfigFilesFromAPI(files []apimodel.AgentConfigFile) []model.AgentConfigFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]model.AgentConfigFile, 0, len(files))
	for _, file := range files {
		out = append(out, model.AgentConfigFile{Path: file.Path, Content: file.Content})
	}
	return out
}

func (s *Service) SetDefaultAgentConfig(ctx context.Context, projectID, configID string) (*model.Project, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apiError(err, "project not found")
	}
	config, err := s.store.GetAgentConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apiError(err, "agent config not found")
	}
	project.DefaultAgentConfigID = config.ID
	if err := s.store.UpsertProject(ctx, project); err != nil {
		return nil, err
	}
	return s.store.GetProject(ctx, projectID)
}

func (s *Service) DeleteAgentConfig(ctx context.Context, projectID, configID string) error {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return apiError(err, "project not found")
	}
	if err := s.store.DeleteAgentConfig(ctx, projectID, configID); err != nil {
		return apiError(err, "agent config not found")
	}
	return nil
}

func apiError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return apperrors.NewStatusError(http.StatusNotFound, notFoundMessage)
	}
	return err
}
