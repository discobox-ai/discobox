package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/store"
	providerregistry "github.com/discobox-ai/discobox/server/providers"
	providerdocker "github.com/discobox-ai/discobox/server/providers/docker"
	providerlibkrun "github.com/discobox-ai/discobox/server/providers/libkrun"
	"github.com/discobox-ai/discobox/version"
	"github.com/discobox-ai/x/id"
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

// EnsureHarnessAvailable fails when a project has no harness config at all,
// naming what was wrong with each built-in image. The rule and its reasoning
// belong to resources/harnessconfigs; the server process calls it after
// InitializeDefaults and refuses to serve when it fails.
func (s *Service) EnsureHarnessAvailable(ctx context.Context, projectID string) error {
	return s.harnessConfigs.EnsureHarnessAvailable(ctx, projectID)
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
	// Decided before the transaction, never inside it: deciding can mean
	// fetching the libkrun image, and it can mean waiting for as long as it
	// takes someone to answer.
	providerType, err := s.chooseDefaultProvider(ctx)
	if err != nil {
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
		defaultProvider := defaultSandboxProvider(projectID, id.NewString(id.PrefixSandboxProvider), providerType)
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
// provider instance and points the project's DefaultPoolID at it, so `discobox
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

// defaultProviderType is the provider a first start installs on this host
// (ADR 0148 §1). Each OS has its VM backend, and Linux's is libkrun in a release
// build. A development build on Linux installs the host's Docker instead: the
// `task dev` loop converges the images it builds onto that daemon, and a
// contributor's machine should not need KVM to run the product. A configured
// choice decides it on Linux, which is the only OS with two to choose from.
func defaultProviderType(configured string, released bool) string {
	switch runtime.GOOS {
	case "linux":
		if configured != "" {
			return configured
		}
		if released {
			return providerlibkrun.ProviderType
		}
		return providerdocker.ProviderType
	case "darwin":
		return "vz"
	case "windows":
		return "wslc"
	default:
		return "unsupported"
	}
}

// chooseDefaultProvider decides the provider a first start installs, and checks
// that it can run here before anything is installed. A provider that cannot is
// never quietly replaced with a weaker one (ADR 0148 §2): the start holds until
// someone chooses, or refuses when nothing can ask.
func (s *Service) chooseDefaultProvider(ctx context.Context) (string, error) {
	providerType := defaultProviderType(s.options.DefaultProvider, version.Released())
	err := providerregistry.CheckDefaultProvider(ctx, providerType, s.options.providerFactoryOptions())
	if err == nil {
		return providerType, nil
	}
	var unavailable *sandbox.ProviderUnavailableError
	if !errors.As(err, &unavailable) || s.options.AwaitDefaultProviderChoice == nil {
		return "", err
	}
	return s.options.AwaitDefaultProviderChoice(ctx, unavailable, []string{providerdocker.ProviderType})
}

// defaultSandboxProvider is the instance a first start installs for
// providerType.
func defaultSandboxProvider(projectID, providerID, providerType string) *model.SandboxProviderInstance {
	provider := &model.SandboxProviderInstance{
		ID:        providerID,
		ProjectID: projectID,
		Type:      providerType,
	}
	switch providerType {
	case providerdocker.ProviderType:
		provider.Name = "Docker"
		provider.Config = defaultDockerProviderConfig()
	case providerlibkrun.ProviderType:
		// Every field of the libkrun config has a default, and the image it
		// boots carries the runtime, so the built-in instance needs no
		// configuration and the host needs nothing installed (ADR 0148 §5).
		provider.Name = "Linux"
	case "vz":
		// Every field of the vz config has a default (see its Definition), so the
		// built-in instance carries no configuration of its own, exactly as on
		// Windows. A Mac therefore gets a working default pool with nothing
		// installed and nothing configured, which is the whole point of ADR 0062:
		// the guest image is pulled from a registry and the pool builds its own
		// images inside the VM it boots.
		provider.Name = "macOS"
	case "wslc":
		// Every field of the wslc config has a default (see its Definition), so
		// the built-in instance carries no configuration of its own.
		provider.Name = "Windows"
	default:
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
