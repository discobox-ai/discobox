package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/obot-platform/discobox/model"
)

type AgentConfigCollectionPathInput struct {
	ProjectID string `path:"projectId" doc:"Project ID"`
}

type AgentConfigDefinitionPathInput struct {
	DefinitionID string `path:"definitionId" doc:"Agent config definition ID"`
}

type AgentConfigPathInput struct {
	ProjectID     string `path:"projectId" doc:"Project ID"`
	AgentConfigID string `path:"agentConfigId" doc:"Agent config ID"`
}

type CreateAgentConfigBody struct {
	DefinitionID   *string         `json:"definitionId,omitempty" doc:"Agent config definition ID to use as defaults"`
	Name           string          `json:"name,omitempty" doc:"Agent config name. Defaults to the definition name when definitionId is provided." maxLength:"200"`
	InstallCommand string          `json:"installCommand,omitempty" doc:"Command used to install the agent"`
	RunCommand     string          `json:"runCommand,omitempty" doc:"Command used to run the agent. Defaults to the definition run command when definitionId is provided."`
	Capabilities   json.RawMessage `json:"capabilities,omitempty" doc:"Agent capabilities or feature metadata"`
}

type UpdateAgentConfigBody struct {
	Name           *string         `json:"name,omitempty" doc:"Agent config name" maxLength:"200"`
	InstallCommand *string         `json:"installCommand,omitempty" doc:"Command used to install the agent"`
	RunCommand     *string         `json:"runCommand,omitempty" doc:"Command used to run the agent"`
	Capabilities   json.RawMessage `json:"capabilities,omitempty" doc:"Agent capabilities or feature metadata"`
}

type CreateAgentConfigInput struct {
	ProjectID string                `path:"projectId" doc:"Project ID"`
	Body      CreateAgentConfigBody `json:"body"`
}

type UpdateAgentConfigInput struct {
	ProjectID     string                `path:"projectId" doc:"Project ID"`
	AgentConfigID string                `path:"agentConfigId" doc:"Agent config ID"`
	Body          UpdateAgentConfigBody `json:"body"`
}

type ListAgentConfigsOutput struct {
	Body ListAgentConfigsBody
}

type ListAgentConfigDefinitionsOutput struct {
	Body ListAgentConfigDefinitionsBody
}

type ListAgentConfigDefinitionsBody struct {
	AgentConfigDefinitions []model.AgentConfigDefinition `json:"agentConfigDefinitions" doc:"Agent config definitions"`
}

type AgentConfigDefinitionOutput struct {
	Body model.AgentConfigDefinition
}

type ListAgentConfigsBody struct {
	AgentConfigs []model.AgentConfig `json:"agentConfigs" doc:"Agent configs"`
}

type AgentConfigOutput struct {
	Body model.AgentConfig
}

type DeleteAgentConfigOutput struct{}

func RegisterAgentConfigOperations(api huma.API, service AgentConfigService) {
	huma.Register(api, huma.Operation{OperationID: "list-agent-config-definitions", Method: http.MethodGet, Path: "/agent-config-definitions", Summary: "List agent config definitions", Tags: []string{"Agent Configs"}}, func(ctx context.Context, _ *struct{}) (*ListAgentConfigDefinitionsOutput, error) {
		definitions, err := service.ListAgentConfigDefinitions(ctx)
		if err != nil {
			return nil, err
		}
		return &ListAgentConfigDefinitionsOutput{Body: ListAgentConfigDefinitionsBody{AgentConfigDefinitions: definitions}}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "get-agent-config-definition", Method: http.MethodGet, Path: "/agent-config-definitions/{definitionId}", Summary: "Get an agent config definition", Tags: []string{"Agent Configs"}}, func(ctx context.Context, input *AgentConfigDefinitionPathInput) (*AgentConfigDefinitionOutput, error) {
		definition, err := service.GetAgentConfigDefinition(ctx, input.DefinitionID)
		if err != nil {
			return nil, err
		}
		return &AgentConfigDefinitionOutput{Body: *definition}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "list-agent-configs", Method: http.MethodGet, Path: "/projects/{projectId}/agent-configs", Summary: "List agent configs", Tags: []string{"Agent Configs"}}, func(ctx context.Context, input *AgentConfigCollectionPathInput) (*ListAgentConfigsOutput, error) {
		configs, err := service.ListAgentConfigs(ctx, input.ProjectID)
		if err != nil {
			return nil, err
		}
		return &ListAgentConfigsOutput{Body: ListAgentConfigsBody{AgentConfigs: configs}}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "create-agent-config", Method: http.MethodPost, Path: "/projects/{projectId}/agent-configs", Summary: "Create an agent config", Tags: []string{"Agent Configs"}}, func(ctx context.Context, input *CreateAgentConfigInput) (*AgentConfigOutput, error) {
		config, err := service.CreateAgentConfig(ctx, input.ProjectID, input.Body)
		if err != nil {
			return nil, err
		}
		return &AgentConfigOutput{Body: *config}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "get-agent-config", Method: http.MethodGet, Path: "/projects/{projectId}/agent-configs/{agentConfigId}", Summary: "Get an agent config", Tags: []string{"Agent Configs"}}, func(ctx context.Context, input *AgentConfigPathInput) (*AgentConfigOutput, error) {
		config, err := service.GetAgentConfig(ctx, input.ProjectID, input.AgentConfigID)
		if err != nil {
			return nil, err
		}
		return &AgentConfigOutput{Body: *config}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "update-agent-config", Method: http.MethodPatch, Path: "/projects/{projectId}/agent-configs/{agentConfigId}", Summary: "Update an agent config", Tags: []string{"Agent Configs"}}, func(ctx context.Context, input *UpdateAgentConfigInput) (*AgentConfigOutput, error) {
		config, err := service.UpdateAgentConfig(ctx, input.ProjectID, input.AgentConfigID, input.Body)
		if err != nil {
			return nil, err
		}
		return &AgentConfigOutput{Body: *config}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "delete-agent-config", Method: http.MethodDelete, Path: "/projects/{projectId}/agent-configs/{agentConfigId}", Summary: "Delete an agent config", Tags: []string{"Agent Configs"}}, func(ctx context.Context, input *AgentConfigPathInput) (*DeleteAgentConfigOutput, error) {
		if err := service.DeleteAgentConfig(ctx, input.ProjectID, input.AgentConfigID); err != nil {
			return nil, err
		}
		return &DeleteAgentConfigOutput{}, nil
	})
}
