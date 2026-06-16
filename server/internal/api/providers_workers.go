package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/obot-platform/discobox/model"
	sandbox "github.com/obot-platform/discobox/sandboxprovider"
)

type SandboxProviderCatalogItem struct {
	ID           string                        `json:"id" doc:"Provider type ID"`
	Name         string                        `json:"name" doc:"Provider display name"`
	Icon         string                        `json:"icon,omitempty" doc:"Provider icon"`
	Description  string                        `json:"description,omitempty" doc:"Provider description"`
	Available    bool                          `json:"available" doc:"Whether provider is available"`
	BuiltIn      bool                          `json:"builtIn" doc:"Whether provider is built in"`
	Capabilities sandbox.ProviderStatus        `json:"capabilities" doc:"Provider capabilities/status"`
	ConfigFields []sandbox.ProviderConfigField `json:"configFields,omitempty" doc:"Provider config fields"`
}

type ListSandboxProviderCatalogOutput struct {
	Body ListSandboxProviderCatalogBody
}

type ListSandboxProviderCatalogBody struct {
	Providers []SandboxProviderCatalogItem `json:"providers" doc:"Provider catalog items"`
}

type ProviderInstanceCollectionPathInput struct {
	ProjectID string `path:"projectId" doc:"Project ID"`
}

type ProviderInstancePathInput struct {
	ProjectID  string `path:"projectId" doc:"Project ID"`
	ProviderID string `path:"providerId" doc:"Provider instance ID"`
}

type CreateSandboxProviderInstanceBody struct {
	Type   string          `json:"type" doc:"Provider type" required:"true"`
	Name   string          `json:"name" doc:"Provider display name" maxLength:"200"`
	Config json.RawMessage `json:"config,omitempty" doc:"Non-secret provider configuration"`
}

type UpdateSandboxProviderInstanceBody struct {
	Name     *string         `json:"name,omitempty" doc:"Provider display name" maxLength:"200"`
	Config   json.RawMessage `json:"config,omitempty" doc:"Non-secret provider configuration"`
	Disabled *bool           `json:"disabled,omitempty" doc:"Whether this provider is disabled"`
}

type CreateSandboxProviderInstanceInput struct {
	ProjectID string                            `path:"projectId" doc:"Project ID"`
	Body      CreateSandboxProviderInstanceBody `json:"body"`
}

type UpdateSandboxProviderInstanceInput struct {
	ProjectID  string                            `path:"projectId" doc:"Project ID"`
	ProviderID string                            `path:"providerId" doc:"Provider instance ID"`
	Body       UpdateSandboxProviderInstanceBody `json:"body"`
}

type ListSandboxProviderInstancesOutput struct {
	Body ListSandboxProviderInstancesBody
}

type ListSandboxProviderInstancesBody struct {
	Providers []model.SandboxProviderInstance `json:"providers" doc:"Provider instances"`
}

type SandboxProviderInstanceOutput struct {
	Body model.SandboxProviderInstance
}

type DeleteSandboxProviderInstanceOutput struct{}

func RegisterSandboxProviderInstanceOperations(api huma.API, service SandboxProviderInstanceService) {
	huma.Register(api, huma.Operation{OperationID: "list-sandbox-provider-catalog", Method: http.MethodGet, Path: "/providers/catalog", Summary: "List sandbox provider catalog", Tags: []string{"Providers"}}, func(ctx context.Context, input *struct{}) (*ListSandboxProviderCatalogOutput, error) {
		providers, err := service.ListSandboxProviderCatalogItems(ctx)
		if err != nil {
			return nil, err
		}
		return &ListSandboxProviderCatalogOutput{Body: ListSandboxProviderCatalogBody{Providers: providers}}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "list-sandbox-provider-instances", Method: http.MethodGet, Path: "/projects/{projectId}/providers", Summary: "List sandbox provider instances", Tags: []string{"Providers"}}, func(ctx context.Context, input *ProviderInstanceCollectionPathInput) (*ListSandboxProviderInstancesOutput, error) {
		providers, err := service.ListSandboxProviderInstances(ctx, input.ProjectID)
		if err != nil {
			return nil, err
		}
		return &ListSandboxProviderInstancesOutput{Body: ListSandboxProviderInstancesBody{Providers: providers}}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "create-sandbox-provider-instance", Method: http.MethodPost, Path: "/projects/{projectId}/providers", Summary: "Create a sandbox provider instance", Tags: []string{"Providers"}}, func(ctx context.Context, input *CreateSandboxProviderInstanceInput) (*SandboxProviderInstanceOutput, error) {
		provider, err := service.CreateSandboxProviderInstance(ctx, input.ProjectID, input.Body)
		if err != nil {
			return nil, err
		}
		return &SandboxProviderInstanceOutput{Body: *provider}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "get-sandbox-provider-instance", Method: http.MethodGet, Path: "/projects/{projectId}/providers/{providerId}", Summary: "Get a sandbox provider instance", Tags: []string{"Providers"}}, func(ctx context.Context, input *ProviderInstancePathInput) (*SandboxProviderInstanceOutput, error) {
		provider, err := service.GetSandboxProviderInstance(ctx, input.ProjectID, input.ProviderID)
		if err != nil {
			return nil, err
		}
		return &SandboxProviderInstanceOutput{Body: *provider}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "update-sandbox-provider-instance", Method: http.MethodPatch, Path: "/projects/{projectId}/providers/{providerId}", Summary: "Update a sandbox provider instance", Tags: []string{"Providers"}}, func(ctx context.Context, input *UpdateSandboxProviderInstanceInput) (*SandboxProviderInstanceOutput, error) {
		provider, err := service.UpdateSandboxProviderInstance(ctx, input.ProjectID, input.ProviderID, input.Body)
		if err != nil {
			return nil, err
		}
		return &SandboxProviderInstanceOutput{Body: *provider}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "delete-sandbox-provider-instance", Method: http.MethodDelete, Path: "/projects/{projectId}/providers/{providerId}", Summary: "Delete a sandbox provider instance", Tags: []string{"Providers"}}, func(ctx context.Context, input *ProviderInstancePathInput) (*DeleteSandboxProviderInstanceOutput, error) {
		if err := service.DeleteSandboxProviderInstance(ctx, input.ProjectID, input.ProviderID); err != nil {
			return nil, err
		}
		return &DeleteSandboxProviderInstanceOutput{}, nil
	})
}

type RegisterWorkerBody struct {
	ProjectID      string `json:"projectId,omitempty"`
	SandboxID      string `json:"sandboxId,omitempty"`
	WorkerID       string `json:"workerId" required:"true"`
	BootstrapToken string `json:"bootstrapToken" required:"true"`
	PublicKey      string `json:"publicKey" required:"true"`
	KeyType        string `json:"keyType,omitempty"`
}

type RegisterWorkerInput struct {
	Body RegisterWorkerBody `json:"body"`
}

type RegisterWorkerResponseBody struct {
	AuthToken string `json:"authToken"`
}

type RegisterWorkerOutput struct {
	Body RegisterWorkerResponseBody
}

type UpdateWorkerStatusBody struct {
	WorkerID    string `json:"workerId" required:"true"`
	Ready       bool   `json:"ready"`
	Schedulable bool   `json:"schedulable"`
	Degraded    bool   `json:"degraded"`
	// AvailableCPUVCPUs is worker-reported spare CPU capacity in vCPUs.
	AvailableCPUVCPUs     float64         `json:"availableCpuVcpus"`
	AvailableMemoryBytes  int64           `json:"availableMemoryBytes"`
	AvailableStorageBytes int64           `json:"availableStorageBytes"`
	Conditions            json.RawMessage `json:"conditions,omitempty"`
}

type UpdateWorkerStatusInput struct {
	Authorization string                 `header:"Authorization" doc:"Bearer worker auth token returned by registration"`
	Body          UpdateWorkerStatusBody `json:"body"`
}

type WorkerOutput struct{ Body model.Worker }

func RegisterWorkerOperations(api huma.API, service WorkerService) {
	huma.Register(api, huma.Operation{OperationID: "register-worker", Method: http.MethodPost, Path: "/api/workers/register", Summary: "Register a bootstrapped worker", Tags: []string{"Workers"}}, func(ctx context.Context, input *RegisterWorkerInput) (*RegisterWorkerOutput, error) {
		resp, err := service.RegisterWorker(ctx, input.Body)
		if err != nil {
			return nil, err
		}
		return &RegisterWorkerOutput{Body: *resp}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "update-worker-status", Method: http.MethodPost, Path: "/api/workers/status", Summary: "Update worker status", Tags: []string{"Workers"}}, func(ctx context.Context, input *UpdateWorkerStatusInput) (*WorkerOutput, error) {
		worker, err := service.UpdateWorkerStatus(ctx, input.Authorization, input.Body)
		if err != nil {
			return nil, err
		}
		return &WorkerOutput{Body: *worker}, nil
	})
}
