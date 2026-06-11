package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/obot-platform/disco2/internal/model"
)

type SandboxCollectionPathInput struct {
	ProjectID string `path:"projectId" doc:"Project ID" format:"uuid"`
}

type SandboxPathInput struct {
	ProjectID string `path:"projectId" doc:"Project ID" format:"uuid"`
	SandboxID string `path:"sandboxId" doc:"Sandbox ID" format:"uuid"`
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

type DeleteSandboxOutput struct {
}

type CreateSandboxBody struct {
	Name               string          `json:"name" doc:"Sandbox name" maxLength:"200" required:"true"`
	Description        *string         `json:"description,omitempty" doc:"Sandbox description"`
	ProviderInstanceID *string         `json:"providerInstanceId,omitempty" doc:"Sandbox provider instance ID"`
	SourceURL          *string         `json:"sourceUrl,omitempty" doc:"Source repository or archive URL" format:"uri"`
	SourceRef          *string         `json:"sourceRef,omitempty" doc:"Source branch, tag, or commit"`
	WorkingDirectory   *string         `json:"workingDirectory,omitempty" doc:"Working directory inside the sandbox"`
	CPUVCPUs           float64         `json:"cpuVcpus,omitempty" doc:"Requested CPU capacity in vCPUs"`
	MemoryBytes        int64           `json:"memoryBytes,omitempty" doc:"Requested memory capacity in bytes"`
	StorageBytes       int64           `json:"storageBytes,omitempty" doc:"Requested storage capacity in bytes"`
	RuntimeState       json.RawMessage `json:"runtimeState,omitempty" doc:"Initial non-secret provider runtime state"`
}

type CreateSandboxInput struct {
	ProjectID string            `path:"projectId" doc:"Project ID" format:"uuid"`
	Body      CreateSandboxBody `json:"body"`
}

type UpdateSandboxBody struct {
	Name               *string         `json:"name,omitempty" doc:"Sandbox name" maxLength:"200"`
	Description        *string         `json:"description,omitempty" doc:"Sandbox description"`
	ProviderInstanceID *string         `json:"providerInstanceId,omitempty" doc:"Sandbox provider instance ID"`
	SourceURL          *string         `json:"sourceUrl,omitempty" doc:"Source repository or archive URL" format:"uri"`
	SourceRef          *string         `json:"sourceRef,omitempty" doc:"Source branch, tag, or commit"`
	WorkingDirectory   *string         `json:"workingDirectory,omitempty" doc:"Working directory inside the sandbox"`
	CPUVCPUs           *float64        `json:"cpuVcpus,omitempty" doc:"Requested CPU capacity in vCPUs"`
	MemoryBytes        *int64          `json:"memoryBytes,omitempty" doc:"Requested memory capacity in bytes"`
	StorageBytes       *int64          `json:"storageBytes,omitempty" doc:"Requested storage capacity in bytes"`
	RuntimeState       json.RawMessage `json:"runtimeState,omitempty" doc:"Non-secret provider runtime state"`
}

type UpdateSandboxInput struct {
	ProjectID string            `path:"projectId" doc:"Project ID" format:"uuid"`
	SandboxID string            `path:"sandboxId" doc:"Sandbox ID" format:"uuid"`
	Body      UpdateSandboxBody `json:"body"`
}

type StartSandboxBody struct {
	Force bool `json:"force,omitempty" doc:"Force start if the provider supports it"`
}

type StartSandboxInput struct {
	ProjectID string           `path:"projectId" doc:"Project ID" format:"uuid"`
	SandboxID string           `path:"sandboxId" doc:"Sandbox ID" format:"uuid"`
	Body      StartSandboxBody `json:"body"`
}

type StopSandboxBody struct {
	Force bool `json:"force,omitempty" doc:"Force stop if the provider supports it"`
}

type StopSandboxInput struct {
	ProjectID string          `path:"projectId" doc:"Project ID" format:"uuid"`
	SandboxID string          `path:"sandboxId" doc:"Sandbox ID" format:"uuid"`
	Body      StopSandboxBody `json:"body"`
}

type RestartSandboxBody struct {
	Force bool `json:"force,omitempty" doc:"Force restart if the provider supports it"`
}

type RestartSandboxInput struct {
	ProjectID string             `path:"projectId" doc:"Project ID" format:"uuid"`
	SandboxID string             `path:"sandboxId" doc:"Sandbox ID" format:"uuid"`
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
		OperationID: "create-sandbox",
		Method:      http.MethodPost,
		Path:        "/projects/{projectId}/sandboxes",
		Summary:     "Create a sandbox",
		Tags:        []string{"Sandboxes"},
	}, func(ctx context.Context, input *CreateSandboxInput) (*SandboxOutput, error) {
		sandbox, err := service.CreateSandbox(ctx, input.ProjectID, input.Body)
		if err != nil {
			return nil, err
		}
		return &SandboxOutput{Body: *sandbox}, nil
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
		OperationID: "delete-sandbox",
		Method:      http.MethodDelete,
		Path:        "/projects/{projectId}/sandboxes/{sandboxId}",
		Summary:     "Delete a sandbox",
		Tags:        []string{"Sandboxes"},
	}, func(ctx context.Context, input *SandboxPathInput) (*DeleteSandboxOutput, error) {
		if err := service.DeleteSandbox(ctx, input.ProjectID, input.SandboxID); err != nil {
			return nil, err
		}
		return &DeleteSandboxOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "start-sandbox",
		Method:      http.MethodPost,
		Path:        "/projects/{projectId}/sandboxes/{sandboxId}/start",
		Summary:     "Start a sandbox",
		Tags:        []string{"Sandboxes"},
	}, func(ctx context.Context, input *StartSandboxInput) (*SandboxOutput, error) {
		sandbox, err := service.StartSandbox(ctx, input.ProjectID, input.SandboxID, input.Body)
		if err != nil {
			return nil, err
		}
		return &SandboxOutput{Body: *sandbox}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "stop-sandbox",
		Method:      http.MethodPost,
		Path:        "/projects/{projectId}/sandboxes/{sandboxId}/stop",
		Summary:     "Stop a sandbox",
		Tags:        []string{"Sandboxes"},
	}, func(ctx context.Context, input *StopSandboxInput) (*SandboxOutput, error) {
		sandbox, err := service.StopSandbox(ctx, input.ProjectID, input.SandboxID, input.Body)
		if err != nil {
			return nil, err
		}
		return &SandboxOutput{Body: *sandbox}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "restart-sandbox",
		Method:      http.MethodPost,
		Path:        "/projects/{projectId}/sandboxes/{sandboxId}/restart",
		Summary:     "Restart a sandbox",
		Tags:        []string{"Sandboxes"},
	}, func(ctx context.Context, input *RestartSandboxInput) (*SandboxOutput, error) {
		sandbox, err := service.RestartSandbox(ctx, input.ProjectID, input.SandboxID, input.Body)
		if err != nil {
			return nil, err
		}
		return &SandboxOutput{Body: *sandbox}, nil
	})
}
