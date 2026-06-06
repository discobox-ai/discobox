// Package service contains application services.
package service

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/obot-platform/disco2/internal/api"
	"github.com/obot-platform/disco2/internal/id"
	"github.com/obot-platform/disco2/internal/model"
)

const (
	defaultUserID    = "00000000-0000-0000-0000-000000000001"
	defaultProjectID = "00000000-0000-0000-0000-000000000002"
)

// Stub is an in-memory implementation used while the real store/provider
// layers are being designed.
type Stub struct {
	mu        sync.Mutex
	user      model.User
	project   model.Project
	sandboxes map[string]model.Sandbox
}

func NewStub() *Stub {
	now := time.Now().UTC()
	user := model.User{
		ID:        defaultUserID,
		Email:     "local@example.com",
		Provider:  "local",
		Subject:   "local",
		CreatedAt: now,
		UpdatedAt: now,
	}
	project := model.Project{
		ID:          defaultProjectID,
		OwnerUserID: user.ID,
		Name:        "Default Project",
		Slug:        "default",
		Owner:       &user,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	return &Stub{
		user:      user,
		project:   project,
		sandboxes: make(map[string]model.Sandbox),
	}
}

func (s *Stub) ListProjects(context.Context) ([]model.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	project := s.projectWithSandboxes()
	return []model.Project{project}, nil
}

func (s *Stub) GetProject(_ context.Context, projectID string) (*model.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return nil, huma.Error404NotFound("project not found")
	}
	project := s.projectWithSandboxes()
	return &project, nil
}

func (s *Stub) ListSandboxes(_ context.Context, projectID string) ([]model.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return nil, huma.Error404NotFound("project not found")
	}
	return s.sortedSandboxes(), nil
}

func (s *Stub) CreateSandbox(_ context.Context, projectID string, input api.CreateSandboxBody) (*model.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return nil, huma.Error404NotFound("project not found")
	}

	now := time.Now().UTC()
	sandbox := model.Sandbox{
		ID:                 id.NewString(),
		ProjectID:          s.project.ID,
		CreatedByUserID:    s.user.ID,
		ProviderInstanceID: input.ProviderInstanceID,
		Name:               input.Name,
		Description:        input.Description,
		ResourceLifecycle:  model.NewResourceLifecycle(model.SandboxCreateOperation, nil),
		SourceURL:          input.SourceURL,
		SourceRef:          input.SourceRef,
		WorkingDirectory:   input.WorkingDirectory,
		RuntimeState:       input.RuntimeState,
		CreatedAt:          now,
		UpdatedAt:          now,
		CreatedBy:          &s.user,
	}
	s.sandboxes[sandbox.ID] = sandbox
	return &sandbox, nil
}

func (s *Stub) GetSandbox(_ context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	return &sandbox, nil
}

func (s *Stub) UpdateSandbox(_ context.Context, projectID, sandboxID string, input api.UpdateSandboxBody) (*model.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return nil, err
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
	if input.RuntimeState != nil {
		sandbox.RuntimeState = input.RuntimeState
	}
	sandbox.UpdatedAt = time.Now().UTC()
	s.sandboxes[sandbox.ID] = sandbox
	return &sandbox, nil
}

func (s *Stub) DeleteSandbox(_ context.Context, projectID, sandboxID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.getSandbox(projectID, sandboxID); err != nil {
		return err
	}
	delete(s.sandboxes, sandboxID)
	return nil
}

func (s *Stub) StartSandbox(_ context.Context, projectID, sandboxID string, _ api.StartSandboxBody) (*model.Sandbox, error) {
	return s.beginSandboxOperation(projectID, sandboxID, model.SandboxStartOperation)
}

func (s *Stub) StopSandbox(_ context.Context, projectID, sandboxID string, _ api.StopSandboxBody) (*model.Sandbox, error) {
	return s.beginSandboxOperation(projectID, sandboxID, model.SandboxStopOperation)
}

func (s *Stub) RestartSandbox(_ context.Context, projectID, sandboxID string, _ api.RestartSandboxBody) (*model.Sandbox, error) {
	return s.beginSandboxOperation(projectID, sandboxID, model.SandboxRestartOperation)
}

func (s *Stub) MaxProjectEventSeq(_ context.Context, projectID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return 0, huma.Error404NotFound("project not found")
	}
	return 0, nil
}

func (s *Stub) ListProjectEventsAfterSeq(_ context.Context, projectID string, _ int64, _ []string) ([]model.ProjectEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return nil, huma.Error404NotFound("project not found")
	}
	return []model.ProjectEvent{}, nil
}

func (s *Stub) ListProjectResourceSnapshots(_ context.Context, projectID string, _ []string, _ int64) ([]model.ProjectEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return nil, huma.Error404NotFound("project not found")
	}
	return []model.ProjectEvent{}, nil
}

func (s *Stub) SubscribeProjectEvents(ctx context.Context, projectID string) (<-chan model.ProjectEvent, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return nil, nil, huma.Error404NotFound("project not found")
	}
	ch := make(chan model.ProjectEvent)
	unsubscribe := func() {
		select {
		case <-ctx.Done():
		default:
			close(ch)
		}
	}
	return ch, unsubscribe, nil
}

func (s *Stub) beginSandboxOperation(projectID, sandboxID string, spec model.OperationSpec) (*model.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	sandbox.BeginOperation(spec, nil)
	sandbox.UpdatedAt = time.Now().UTC()
	s.sandboxes[sandbox.ID] = sandbox
	return &sandbox, nil
}

func (s *Stub) getSandbox(projectID, sandboxID string) (model.Sandbox, error) {
	if projectID != s.project.ID {
		return model.Sandbox{}, huma.Error404NotFound("project not found")
	}
	sandbox, ok := s.sandboxes[sandboxID]
	if !ok {
		return model.Sandbox{}, huma.Error404NotFound("sandbox not found")
	}
	sandbox.CreatedBy = &s.user
	return sandbox, nil
}

func (s *Stub) projectWithSandboxes() model.Project {
	project := s.project
	project.Owner = &s.user
	project.Sandboxes = s.sortedSandboxes()
	return project
}

func (s *Stub) sortedSandboxes() []model.Sandbox {
	sandboxes := make([]model.Sandbox, 0, len(s.sandboxes))
	for _, sandbox := range s.sandboxes {
		sandbox.CreatedBy = &s.user
		sandboxes = append(sandboxes, sandbox)
	}
	slices.SortFunc(sandboxes, func(a, b model.Sandbox) int {
		if a.CreatedAt.Before(b.CreatedAt) {
			return -1
		}
		if a.CreatedAt.After(b.CreatedAt) {
			return 1
		}
		return 0
	})
	return sandboxes
}
