package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/obot-platform/discobox/internal/model"
)

type SandboxCollectionPathInput struct {
	ProjectID string `path:"projectId" doc:"Project ID"`
}

type SandboxPathInput struct {
	ProjectID string `path:"projectId" doc:"Project ID"`
	SandboxID string `path:"sandboxId" doc:"Sandbox ID"`
}

type ListSandboxesOutput struct {
	Body ListSandboxesBody
}

type ListSandboxesBody struct {
	Sandboxes []model.Sandbox `json:"sandboxes" doc:"Sandboxes"`
}

type SandboxOutput struct {
	Body model.Sandbox
}

type AcceptedSandboxOutput struct {
	Status int `status:"202"`
	Body   model.Sandbox
}

type DeleteSandboxOutput struct {
	Status int `status:"202"`
}

type CreateSandboxBody struct {
	Name                     string                     `json:"name" doc:"Sandbox name" maxLength:"200" required:"true"`
	Description              *string                    `json:"description,omitempty" doc:"Sandbox description"`
	ProviderInstanceID       *string                    `json:"providerInstanceId,omitempty" doc:"Sandbox provider instance ID"`
	AgentConfigID            *string                    `json:"agentConfigId,omitempty" doc:"Agent config ID"`
	AgentName                *string                    `json:"agentName,omitempty" doc:"Agent config name to resolve at create time"`
	AgentModel               *string                    `json:"agentModel,omitempty" doc:"Model the agent should use"`
	AgentModelServiceTier    *string                    `json:"agentModelServiceTier,omitempty" doc:"Model service tier the agent should use"`
	AgentModelReasoningLevel *string                    `json:"agentModelReasoningLevel,omitempty" doc:"Model reasoning level the agent should use"`
	Prompt                   *string                    `json:"prompt,omitempty" doc:"Prompt the agent should run"`
	SourceURL                *string                    `json:"sourceUrl,omitempty" doc:"Git source URL" format:"uri"`
	SourceRef                *string                    `json:"sourceRef,omitempty" doc:"Git source branch, tag, or commit"`
	SourceRefType            *string                    `json:"sourceRefType,omitempty" doc:"Git source ref type, such as branch, tag, or commit"`
	SourceDirectory          *string                    `json:"sourceDirectory,omitempty" doc:"Directory where the main source should be placed inside the sandbox"`
	WorkingDirectory         *string                    `json:"workingDirectory,omitempty" doc:"Working directory inside the sandbox"`
	SourceCodeReferences     model.SourceCodeReferences `json:"sourceCodeReferences,omitempty" doc:"Map of sandbox directories to additional source code Git references"`
	UserUID                  *int                       `json:"userUid,omitempty" doc:"UID to use inside the sandbox"`
	UserGID                  *int                       `json:"userGid,omitempty" doc:"GID to use inside the sandbox"`
	CPUVCPUs                 float64                    `json:"cpuVcpus,omitempty" doc:"Requested CPU capacity in vCPUs"`
	MemoryBytes              int64                      `json:"memoryBytes,omitempty" doc:"Requested memory capacity in bytes"`
	StorageBytes             int64                      `json:"storageBytes,omitempty" doc:"Requested storage capacity in bytes"`
}

type CreateSandboxInput struct {
	ProjectID string            `path:"projectId" doc:"Project ID"`
	Body      CreateSandboxBody `json:"body"`
}

type UpdateSandboxBody struct {
	Name *string `json:"name,omitempty" doc:"Sandbox name" maxLength:"200"`
}

type UpdateSandboxInput struct {
	ProjectID string            `path:"projectId" doc:"Project ID"`
	SandboxID string            `path:"sandboxId" doc:"Sandbox ID"`
	Body      UpdateSandboxBody `json:"body"`
}

type StartSandboxBody struct {
	Force bool `json:"force,omitempty" doc:"Force start if the provider supports it"`
}

type StartSandboxInput struct {
	ProjectID string           `path:"projectId" doc:"Project ID"`
	SandboxID string           `path:"sandboxId" doc:"Sandbox ID"`
	Body      StartSandboxBody `json:"body"`
}

type StopSandboxBody struct {
	Force bool `json:"force,omitempty" doc:"Force stop if the provider supports it"`
}

type StopSandboxInput struct {
	ProjectID string          `path:"projectId" doc:"Project ID"`
	SandboxID string          `path:"sandboxId" doc:"Sandbox ID"`
	Body      StopSandboxBody `json:"body"`
}

type RestartSandboxBody struct {
	Force bool `json:"force,omitempty" doc:"Force restart if the provider supports it"`
}

type RestartSandboxInput struct {
	ProjectID string             `path:"projectId" doc:"Project ID"`
	SandboxID string             `path:"sandboxId" doc:"Sandbox ID"`
	Body      RestartSandboxBody `json:"body"`
}

// RegisterSandboxOperations registers CRUD and lifecycle operations for sandboxes.
func RegisterSandboxOperations(api huma.API, service SandboxService) {
	huma.Register(api, huma.Operation{
		OperationID: "list-sandboxes",
		Method:      http.MethodGet,
		Path:        "/projects/{projectId}/sandboxes",
		Summary:     "List sandboxes",
		Tags:        []string{"Sandboxes"},
	}, func(ctx context.Context, input *SandboxCollectionPathInput) (*ListSandboxesOutput, error) {
		sandboxes, err := service.ListSandboxes(ctx, input.ProjectID)
		if err != nil {
			return nil, err
		}
		return &ListSandboxesOutput{Body: ListSandboxesBody{Sandboxes: sandboxes}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-sandbox",
		Method:        http.MethodPost,
		Path:          "/projects/{projectId}/sandboxes",
		DefaultStatus: http.StatusAccepted,
		Summary:       "Create a sandbox",
		Tags:          []string{"Sandboxes"},
	}, func(ctx context.Context, input *CreateSandboxInput) (*AcceptedSandboxOutput, error) {
		sandbox, err := service.CreateSandbox(ctx, input.ProjectID, input.Body)
		if err != nil {
			return nil, err
		}
		return &AcceptedSandboxOutput{Status: http.StatusAccepted, Body: *sandbox}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-sandbox",
		Method:      http.MethodGet,
		Path:        "/projects/{projectId}/sandboxes/{sandboxId}",
		Summary:     "Get a sandbox",
		Tags:        []string{"Sandboxes"},
	}, func(ctx context.Context, input *SandboxPathInput) (*SandboxOutput, error) {
		sandbox, err := service.GetSandbox(ctx, input.ProjectID, input.SandboxID)
		if err != nil {
			return nil, err
		}
		return &SandboxOutput{Body: *sandbox}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-sandbox",
		Method:      http.MethodPatch,
		Path:        "/projects/{projectId}/sandboxes/{sandboxId}",
		Summary:     "Update a sandbox",
		Tags:        []string{"Sandboxes"},
	}, func(ctx context.Context, input *UpdateSandboxInput) (*SandboxOutput, error) {
		sandbox, err := service.UpdateSandbox(ctx, input.ProjectID, input.SandboxID, input.Body)
		if err != nil {
			return nil, err
		}
		return &SandboxOutput{Body: *sandbox}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-sandbox",
		Method:        http.MethodDelete,
		Path:          "/projects/{projectId}/sandboxes/{sandboxId}",
		DefaultStatus: http.StatusAccepted,
		Summary:       "Delete a sandbox",
		Tags:          []string{"Sandboxes"},
	}, func(ctx context.Context, input *SandboxPathInput) (*DeleteSandboxOutput, error) {
		if err := service.DeleteSandbox(ctx, input.ProjectID, input.SandboxID); err != nil {
			return nil, err
		}
		return &DeleteSandboxOutput{Status: http.StatusAccepted}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "start-sandbox",
		Method:        http.MethodPost,
		Path:          "/projects/{projectId}/sandboxes/{sandboxId}/start",
		DefaultStatus: http.StatusAccepted,
		Summary:       "Start a sandbox",
		Tags:          []string{"Sandboxes"},
	}, func(ctx context.Context, input *StartSandboxInput) (*AcceptedSandboxOutput, error) {
		sandbox, err := service.StartSandbox(ctx, input.ProjectID, input.SandboxID, input.Body)
		if err != nil {
			return nil, err
		}
		return &AcceptedSandboxOutput{Status: http.StatusAccepted, Body: *sandbox}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "stop-sandbox",
		Method:        http.MethodPost,
		Path:          "/projects/{projectId}/sandboxes/{sandboxId}/stop",
		DefaultStatus: http.StatusAccepted,
		Summary:       "Stop a sandbox",
		Tags:          []string{"Sandboxes"},
	}, func(ctx context.Context, input *StopSandboxInput) (*AcceptedSandboxOutput, error) {
		sandbox, err := service.StopSandbox(ctx, input.ProjectID, input.SandboxID, input.Body)
		if err != nil {
			return nil, err
		}
		return &AcceptedSandboxOutput{Status: http.StatusAccepted, Body: *sandbox}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "restart-sandbox",
		Method:        http.MethodPost,
		Path:          "/projects/{projectId}/sandboxes/{sandboxId}/restart",
		DefaultStatus: http.StatusAccepted,
		Summary:       "Restart a sandbox",
		Tags:          []string{"Sandboxes"},
	}, func(ctx context.Context, input *RestartSandboxInput) (*AcceptedSandboxOutput, error) {
		sandbox, err := service.RestartSandbox(ctx, input.ProjectID, input.SandboxID, input.Body)
		if err != nil {
			return nil, err
		}
		return &AcceptedSandboxOutput{Status: http.StatusAccepted, Body: *sandbox}, nil
	})
}
