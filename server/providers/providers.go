package providers

import (
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport/carrierhub"
	"github.com/obot-platform/discobox/server/providers/digitalocean"
	"github.com/obot-platform/discobox/server/providers/docker"
	"github.com/obot-platform/discobox/server/providers/dockerworker"
	"github.com/obot-platform/discobox/server/providers/execvm"
	"github.com/obot-platform/discobox/server/providers/libkrun"
	"github.com/obot-platform/discobox/server/providers/poolruntime"
)

type FactoryOptions struct {
	DevelopmentImageSync *dockerworker.DevelopmentImageSynchronizer
	// ControlPlaneStreams receives connections that a pool guest opens toward
	// the control plane. Only backends whose transport cannot be dialed inward
	// use it; the server serves its ordinary handler over whatever arrives.
	ControlPlaneStreams *carrierhub.Hub
}

func RegisterBuiltInSandboxProviderFactories(manager *sandbox.ProviderManager, poolManager poolruntime.PoolManager, options FactoryOptions) {
	if manager == nil {
		return
	}
	manager.RegisterProviderDefinition(digitalocean.ProviderType, digitalocean.Definition())
	manager.RegisterFactory(digitalocean.ProviderType, digitalocean.FactoryWithPoolManager(poolManager, options.DevelopmentImageSync))
	manager.RegisterProviderConfigValidator(digitalocean.ProviderType, digitalocean.Validate)
	manager.RegisterProviderDefinition(docker.ProviderType, docker.Definition())
	manager.RegisterFactory(docker.ProviderType, docker.FactoryWithPoolManager(poolManager, options.DevelopmentImageSync))
	manager.RegisterProviderConfigValidator(docker.ProviderType, docker.Validate)
	manager.RegisterProviderDefinition(execvm.ProviderType, execvm.Definition())
	manager.RegisterFactory(execvm.ProviderType, execvm.FactoryWithPoolManager(poolManager, options.DevelopmentImageSync))
	manager.RegisterProviderConfigValidator(execvm.ProviderType, execvm.Validate)
	manager.RegisterProviderDefinition(libkrun.ProviderType, libkrun.Definition())
	manager.RegisterFactory(libkrun.ProviderType, libkrun.FactoryWithPoolManager(poolManager, options.DevelopmentImageSync))
	manager.RegisterProviderConfigValidator(libkrun.ProviderType, libkrun.Validate)
	registerPlatformProviderFactories(manager, poolManager, options)
}
