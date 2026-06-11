package sandboxservice

import (
	"context"
	"fmt"
	"strings"

	"github.com/obot-platform/disco2/internal/api"
	"github.com/obot-platform/disco2/internal/id"
	"github.com/obot-platform/disco2/internal/model"
)

func (s *Service) ListSandboxes(ctx context.Context, projectID string) ([]model.Sandbox, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, mapAPIError(err, "project not found")
	}
	return s.store.ListSandboxes(ctx, projectID)
}

func (s *Service) CreateSandbox(ctx context.Context, projectID string, input api.CreateSandboxBody) (*model.Sandbox, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, mapAPIError(err, "project not found")
	}
	providerID := input.ProviderInstanceID
	if providerID == nil && project.DefaultSandboxProviderID != "" {
		id := project.DefaultSandboxProviderID
		providerID = &id
	}
	if providerID != nil {
		provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, *providerID)
		if err != nil {
			return nil, mapAPIError(err, "provider instance not found")
		}
		if provider.Disabled {
			return nil, fmt.Errorf("provider instance disabled")
		}
	}

	if strings.TrimSpace(input.Name) == "" {
		return nil, fmt.Errorf("sandbox name is required")
	}
	sandboxID, err := id.New()
	if err != nil {
		return nil, err
	}
	sandbox := &model.Sandbox{
		ID:                 sandboxID,
		ProjectID:          projectID,
		CreatedByUserID:    s.defaultUserID,
		ProviderInstanceID: providerID,
		Name:               input.Name,
		Description:        input.Description,
		ResourceLifecycle:  model.NewResourceLifecycle(model.SandboxCreateOperation, nil),
		SourceURL:          input.SourceURL,
		SourceRef:          input.SourceRef,
		WorkingDirectory:   input.WorkingDirectory,
		CPUVCPUs:           input.CPUVCPUs,
		MemoryBytes:        input.MemoryBytes,
		StorageBytes:       input.StorageBytes,
		RuntimeState:       input.RuntimeState,
	}
	return s.sandboxes.Create(ctx, sandbox)
}

func (s *Service) GetSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	return sandbox, nil
}

func (s *Service) UpdateSandbox(ctx context.Context, projectID, sandboxID string, input api.UpdateSandboxBody) (*model.Sandbox, error) {
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}

	if input.Name != nil {
		sandbox.Name = *input.Name
	}
	if input.Description != nil {
		sandbox.Description = input.Description
	}
	if input.ProviderInstanceID != nil {
		sandbox.ProviderInstanceID = input.ProviderInstanceID
	}
	if input.SourceURL != nil {
		sandbox.SourceURL = input.SourceURL
	}
	if input.SourceRef != nil {
		sandbox.SourceRef = input.SourceRef
	}
	if input.WorkingDirectory != nil {
		sandbox.WorkingDirectory = input.WorkingDirectory
	}
	if input.CPUVCPUs != nil {
		sandbox.CPUVCPUs = *input.CPUVCPUs
	}
	if input.MemoryBytes != nil {
		sandbox.MemoryBytes = *input.MemoryBytes
	}
	if input.StorageBytes != nil {
		sandbox.StorageBytes = *input.StorageBytes
	}
	if input.RuntimeState != nil {
		sandbox.RuntimeState = input.RuntimeState
	}

	if err := s.store.UpdateSandbox(ctx, sandbox); err != nil {
		return nil, err
	}
	return s.store.GetSandbox(ctx, projectID, sandboxID)
}

func (s *Service) DeleteSandbox(ctx context.Context, projectID, sandboxID string) error {
	_, err := s.beginSandboxOperation(ctx, projectID, sandboxID, model.SandboxDeleteOperation)
	return err
}

func (s *Service) StartSandbox(ctx context.Context, projectID, sandboxID string, _ api.StartSandboxBody) (*model.Sandbox, error) {
	return s.beginSandboxOperation(ctx, projectID, sandboxID, model.SandboxStartOperation)
}

func (s *Service) StopSandbox(ctx context.Context, projectID, sandboxID string, _ api.StopSandboxBody) (*model.Sandbox, error) {
	return s.beginSandboxOperation(ctx, projectID, sandboxID, model.SandboxStopOperation)
}

func (s *Service) RestartSandbox(ctx context.Context, projectID, sandboxID string, _ api.RestartSandboxBody) (*model.Sandbox, error) {
	return s.beginSandboxOperation(ctx, projectID, sandboxID, model.SandboxRestartOperation, func(sandbox *model.Sandbox) {
		sandbox.RestartGeneration++
	})
}

func (s *Service) beginSandboxOperation(ctx context.Context, projectID, sandboxID string, spec model.OperationSpec, mutate ...func(*model.Sandbox)) (*model.Sandbox, error) {
	sandbox, err := s.sandboxes.Begin(ctx, projectID, sandboxID, spec, mutate...)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	return sandbox, nil
}
