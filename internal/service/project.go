package service

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"time"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/internal/authctx"
	"github.com/obot-platform/discobox/internal/model"
	"github.com/obot-platform/discobox/internal/store"
)

const (
	DefaultProviderInstanceID        = "00000000-0000-0000-0000-000000000003"
	defaultProviderInstalledStateKey = "defaults.default_sandbox_provider.installed"
)

type InitializeDefaultsOption func(*initializeDefaultsOptions)

type initializeDefaultsOptions struct {
	skipProvider bool
}

func WithoutDefaultProviderInstallation() InitializeDefaultsOption {
	return func(opts *initializeDefaultsOptions) {
		opts.skipProvider = true
	}
}

// InitializeDefaults creates the single default project used before
// user/project management APIs exist.
func (s *Service) InitializeDefaults(ctx context.Context, tenantID, userID string, options ...InitializeDefaultsOption) error {
	var opts initializeDefaultsOptions
	for _, option := range options {
		if option != nil {
			option(&opts)
		}
	}
	now := time.Now().UTC()
	project := &model.Project{
		ID:          DefaultProjectID,
		TenantID:    tenantID,
		OwnerUserID: userID,
		Name:        "Default Project",
		Slug:        "default",
		Default:     true,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if _, err := s.store.CreateProjectIfNotExists(ctx, project); err != nil {
		return err
	}
	if _, err := s.store.CreateProjectMemberIfNotExists(ctx, &model.ProjectMember{
		ProjectID: DefaultProjectID,
		UserID:    userID,
		Role:      "owner",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		return err
	}
	if opts.skipProvider {
		return nil
	}
	return s.ensureDefaultSandboxProviderInstalled(ctx)
}

func (s *Service) ensureDefaultSandboxProviderInstalled(ctx context.Context) error {
	if _, err := s.store.GetServerState(ctx, defaultProviderInstalledStateKey); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	defaultProvider := defaultSandboxProviderForOS()
	return s.store.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
		if _, err := txStore.GetServerState(ctx, defaultProviderInstalledStateKey); err == nil {
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}

		project, err := txStore.GetProject(ctx, DefaultProjectID)
		if err != nil {
			return err
		}
		if _, err := txStore.GetSandboxProviderInstance(ctx, DefaultProjectID, defaultProvider.ID); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if err := txStore.CreateSandboxProviderInstance(ctx, defaultProvider); err != nil {
				return err
			}
		}
		if project.DefaultSandboxProviderID == "" {
			project.DefaultSandboxProviderID = defaultProvider.ID
			if err := txStore.UpsertProject(ctx, project); err != nil {
				return err
			}
		}

		value, err := json.Marshal(map[string]any{
			"installed":          true,
			"os":                 runtime.GOOS,
			"providerInstanceId": defaultProvider.ID,
			"providerType":       defaultProvider.Type,
		})
		if err != nil {
			return err
		}
		return txStore.CreateServerState(ctx, &model.ServerState{
			Key:   defaultProviderInstalledStateKey,
			Value: value,
		})
	})
}

func defaultSandboxProviderForOS() *model.SandboxProviderInstance {
	provider := &model.SandboxProviderInstance{
		ID:        DefaultProviderInstanceID,
		ProjectID: DefaultProjectID,
		BuiltIn:   true,
	}
	switch runtime.GOOS {
	case "linux":
		provider.Type = "docker"
		provider.Name = "Docker"
	case "darwin":
		provider.Type = "macos"
		provider.Name = "macOS"
		provider.Disabled = true
	case "windows":
		provider.Type = "windows"
		provider.Name = "Windows"
		provider.Disabled = true
	default:
		provider.Type = "unsupported"
		provider.Name = runtime.GOOS
		provider.Disabled = true
	}
	return provider
}

func (s *Service) ListProjects(ctx context.Context) ([]model.Project, error) {
	if userID, err := authctx.UserID(ctx); err == nil {
		return s.store.ListProjectsForUser(ctx, userID)
	}
	return s.store.ListProjects(ctx)
}

func (s *Service) GetProject(ctx context.Context, projectID string) (*model.Project, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apiError(err, "project not found")
	}
	return project, nil
}
