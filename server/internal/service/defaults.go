package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/store"
	providerdocker "github.com/obot-platform/discobox/server/providers/docker"
)

const (
	DefaultProjectID                 = "prj_default"
	DefaultProviderInstanceID        = "prv_default"
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

// InitializeDefaults creates the built-in local identity and the single default
// project used before user/project management APIs exist.
func (s *Service) InitializeDefaults(ctx context.Context, userID string, options ...InitializeDefaultsOption) error {
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
		return err
	}
	project := &model.Project{
		ID:          DefaultProjectID,
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
	defaultProvider := defaultSandboxProviderForOS()
	if _, err := s.store.GetServerState(ctx, defaultProviderInstalledStateKey); err == nil {
		return s.ensureDefaultSandboxProviderConfig(ctx, defaultProvider)
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	return s.store.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
		if _, err := txStore.GetServerState(ctx, defaultProviderInstalledStateKey); err == nil {
			return ensureDefaultSandboxProviderConfig(ctx, txStore, defaultProvider)
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
			if err := txStore.RestoreSandboxProviderInstance(ctx, defaultProvider); err != nil {
				if !errors.Is(err, store.ErrNotFound) {
					return err
				}
				if err := txStore.CreateSandboxProviderInstance(ctx, defaultProvider); err != nil {
					return err
				}
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

func (s *Service) ensureDefaultSandboxProviderConfig(ctx context.Context, defaultProvider *model.SandboxProviderInstance) error {
	return ensureDefaultSandboxProviderConfig(ctx, s.store, defaultProvider)
}

func ensureDefaultSandboxProviderConfig(ctx context.Context, appStore *store.Store, defaultProvider *model.SandboxProviderInstance) error {
	if defaultProvider == nil || len(defaultProvider.Config) == 0 {
		return nil
	}
	provider, err := appStore.GetSandboxProviderInstance(ctx, defaultProvider.ProjectID, defaultProvider.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if !provider.BuiltIn {
		return nil
	}
	config := defaultProvider.Config
	if len(provider.Config) > 0 {
		config = provider.Config
		if shouldUpdateDefaultProviderConfig(provider.Config, defaultProvider.Config) {
			config = mergeDefaultProviderConfig(provider.Config, defaultProvider.Config)
		}
	}
	if provider.Type == "docker" {
		config = removeLegacyDefaultDockerProviderImage(config)
	}
	if string(config) == string(provider.Config) {
		return nil
	}
	provider.Config = config
	return appStore.UpdateSandboxProviderInstance(ctx, provider)
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
		provider.Config = defaultDockerProviderConfig()
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

func defaultDockerProviderConfig() json.RawMessage {
	systemd := true
	config := map[string]any{
		"bindDockerSocket":  "/var/run/docker.sock",
		"agentPort":         providerdocker.DefaultAgentPort(),
		"systemd":           systemd,
		"minWorkers":        1,
		"maxWorkers":        1,
		"minHealthyWorkers": 1,
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

func shouldUpdateDefaultProviderConfig(current, defaults json.RawMessage) bool {
	var currentValue, defaultValue map[string]any
	if err := json.Unmarshal(current, &currentValue); err != nil {
		return false
	}
	if err := json.Unmarshal(defaults, &defaultValue); err != nil {
		return false
	}
	for key := range defaultValue {
		_, ok := currentValue[key]
		if !ok {
			return true
		}
	}
	return false
}

func mergeDefaultProviderConfig(current, defaults json.RawMessage) json.RawMessage {
	var currentValue, defaultValue map[string]any
	if err := json.Unmarshal(current, &currentValue); err != nil {
		return current
	}
	if err := json.Unmarshal(defaults, &defaultValue); err != nil {
		return current
	}
	for key, defaultField := range defaultValue {
		_, ok := currentValue[key]
		if !ok {
			currentValue[key] = defaultField
		}
	}
	data, err := json.Marshal(currentValue)
	if err != nil {
		return current
	}
	return data
}

func removeLegacyDefaultDockerProviderImage(config json.RawMessage) json.RawMessage {
	var value map[string]any
	if err := json.Unmarshal(config, &value); err != nil {
		return config
	}
	image, _ := value["image"].(string)
	if !legacyDefaultDockerProviderImage(image) {
		return config
	}
	delete(value, "image")
	data, err := json.Marshal(value)
	if err != nil {
		return config
	}
	return data
}

func legacyDefaultDockerProviderImage(image string) bool {
	image = strings.TrimSpace(image)
	return image == providerdocker.DefaultImage() ||
		image == "discobox-worker-agent:local" ||
		strings.HasPrefix(image, "discobox-worker-agent:dev-")
}
