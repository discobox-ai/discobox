package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"time"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/id"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/store"
	providerdocker "github.com/obot-platform/discobox/server/providers/docker"
)

const defaultProviderInstalledStateKey = "defaults.default_sandbox_provider.installed"

type InitializeDefaultsOption func(*initializeDefaultsOptions)

type initializeDefaultsOptions struct {
	skipProvider bool
}

func WithoutDefaultProviderInstallation() InitializeDefaultsOption {
	return func(opts *initializeDefaultsOptions) {
		opts.skipProvider = true
	}
}

// InitializeDefaults creates the built-in local identity and the single default
// project used before user/project management APIs exist. It returns the
// default project, creating it with a generated ID on first boot and
// resolving the existing row (by user membership + the Default flag, not by a
// fixed ID) on subsequent boots.
func (s *Service) InitializeDefaults(ctx context.Context, userID string, options ...InitializeDefaultsOption) (*model.Project, error) {
	var opts initializeDefaultsOptions
	for _, option := range options {
		if option != nil {
			option(&opts)
		}
	}
	now := time.Now().UTC()
	if err := s.store.UpsertUser(ctx, &model.User{
		ID:        userID,
		Email:     "local@example.com",
		Provider:  "default",
		Subject:   "default",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		return nil, err
	}
	project, err := s.store.GetDefaultProjectForUser(ctx, userID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		project, err = s.store.CreateProjectIfNotExists(ctx, &model.Project{
			ID:          id.NewString(id.PrefixProject),
			OwnerUserID: userID,
			Name:        "Default Project",
			Slug:        "default",
			Default:     true,
			CreatedAt:   now,
			UpdatedAt:   now,
		})
		if err != nil {
			return nil, err
		}
	}
	if _, err := s.store.CreateProjectMemberIfNotExists(ctx, &model.ProjectMember{
		ProjectID: project.ID,
		UserID:    userID,
		Role:      "owner",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		return nil, err
	}
	// Seed the included harnesses and keep their images current. They start
	// unconfigured, so they are visible but not selectable until configured.
	if err := s.harnessConfigs.SeedBuiltIns(ctx, project.ID); err != nil {
		return nil, err
	}
	if opts.skipProvider {
		return project, nil
	}
	if err := s.ensureDefaultSandboxProviderInstalled(ctx, project.ID); err != nil {
		return nil, err
	}
	return project, nil
}

// ensureDefaultSandboxProviderInstalled seeds the default sandbox provider and
// its pool exactly once, gated on a server_state row rather than on the records
// themselves. After seeding they are ordinary user-owned records: editing or
// deleting either is a normal, permanent action the server never undoes.
func (s *Service) ensureDefaultSandboxProviderInstalled(ctx context.Context, projectID string) error {
	if _, err := s.store.GetServerState(ctx, defaultProviderInstalledStateKey); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	return s.store.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
		if _, err := txStore.GetServerState(ctx, defaultProviderInstalledStateKey); err == nil {
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}

		if _, err := txStore.GetProject(ctx, projectID); err != nil {
			return err
		}
		defaultProvider := defaultSandboxProviderForOS(projectID, id.NewString(id.PrefixSandboxProvider))
		if err := txStore.CreateSandboxProviderInstance(ctx, defaultProvider); err != nil {
			return err
		}
		if err := ensureDefaultPool(ctx, txStore, defaultProvider); err != nil {
			return err
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

// ensureDefaultPool seeds the project's default pool against the built-in
// provider instance and points the project's DefaultPoolID at it, so `disco
// run` works with zero configuration. It runs exactly once, in the same
// transaction that installs the default provider — if the user later deletes
// the pool (or the provider), it is not recreated.
func ensureDefaultPool(ctx context.Context, appStore *store.Store, defaultProvider *model.SandboxProviderInstance) error {
	pool := &model.Pool{
		ID:        id.NewString(id.PrefixPool),
		ProjectID: defaultProvider.ProjectID,
		PoolManifest: model.PoolManifest{
			Name:               "Default",
			ProviderInstanceID: defaultProvider.ID,
		},
	}
	if err := appStore.CreatePool(ctx, pool); err != nil {
		return err
	}
	project, err := appStore.GetProject(ctx, defaultProvider.ProjectID)
	if err != nil {
		return err
	}
	project.DefaultPoolID = pool.ID
	return appStore.UpsertProject(ctx, project)
}

func defaultSandboxProviderForOS(projectID, providerID string) *model.SandboxProviderInstance {
	provider := &model.SandboxProviderInstance{
		ID:        providerID,
		ProjectID: projectID,
	}
	switch runtime.GOOS {
	case "linux":
		provider.Type = "docker"
		provider.Name = "Docker"
		provider.Config = defaultDockerProviderConfig()
	case "darwin":
		provider.Type = "macos"
		provider.Name = "macOS"
		provider.Disabled = true
	case "windows":
		// Every field of the wslc config has a default (see its Definition), so
		// the built-in instance carries no configuration of its own.
		provider.Type = "wslc"
		provider.Name = "Windows"
	default:
		provider.Type = "unsupported"
		provider.Name = runtime.GOOS
		provider.Disabled = true
	}
	return provider
}

func defaultDockerProviderConfig() json.RawMessage {
	config := map[string]any{
		"bindDockerSocket": "/var/run/docker.sock",
		"agentPort":        providerdocker.DefaultAgentPort(),
	}
	if hostMounts := defaultDockerHostMounts(); len(hostMounts) > 0 {
		config["hostMounts"] = hostMounts
	}
	data, err := json.Marshal(config)
	if err != nil {
		return nil
	}
	return data
}

func defaultDockerHostMounts() []providerdocker.HostMount {
	candidates := []string{"/home", "/Users"}
	mounts := make([]providerdocker.HostMount, 0, len(candidates))
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil || !info.IsDir() {
			continue
		}
		mounts = append(mounts, providerdocker.HostMount{Source: candidate, ReadOnly: true})
	}
	return mounts
}
