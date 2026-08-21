package providers

import (
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport/carrierhub"
	"github.com/discobox-ai/discobox/server/providers/digitalocean"
	"github.com/discobox-ai/discobox/server/providers/docker"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/execvm"
	"github.com/discobox-ai/discobox/server/providers/libkrun"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

type FactoryOptions struct {
	DevelopmentImageSync *dockerworker.DevelopmentImageSynchronizer
	// ControlPlaneStreams receives connections that a pool guest opens toward
	// the control plane. Only backends whose transport cannot be dialed inward
	// use it; the server serves its ordinary handler over whatever arrives.
	ControlPlaneStreams *carrierhub.Hub
	// ListenEndpoints are the endpoints the control plane listens on. A backend
	// whose agent dials inward picks its address from them.
	ListenEndpoints []string
}

func RegisterBuiltInSandboxProviderFactories(manager *sandbox.ProviderManager, poolManager poolruntime.PoolManager, options FactoryOptions) {
	if manager == nil {
		return
	}
	manager.RegisterProviderDefinition(digitalocean.ProviderType, digitalocean.Definition())
	manager.RegisterFactory(digitalocean.ProviderType, digitalocean.FactoryWithPoolManager(poolManager, options.DevelopmentImageSync))
	manager.RegisterProviderConfigValidator(digitalocean.ProviderType, digitalocean.Validate)
	manager.RegisterProviderDefinition(docker.ProviderType, docker.Definition())
	manager.RegisterFactory(docker.ProviderType, docker.FactoryWithPoolManager(poolManager, options.DevelopmentImageSync, options.ListenEndpoints))
	manager.RegisterProviderConfigValidator(docker.ProviderType, docker.Validate)
	manager.RegisterProviderDefinition(execvm.ProviderType, execvm.Definition())
	manager.RegisterFactory(execvm.ProviderType, execvm.FactoryWithPoolManager(poolManager, options.DevelopmentImageSync))
	manager.RegisterProviderConfigValidator(execvm.ProviderType, execvm.Validate)
	manager.RegisterProviderDefinition(libkrun.ProviderType, libkrun.Definition())
	manager.RegisterFactory(libkrun.ProviderType, libkrun.FactoryWithPoolManager(poolManager, options.DevelopmentImageSync))
	manager.RegisterProviderConfigValidator(libkrun.ProviderType, libkrun.Validate)
	registerPlatformProviderFactories(manager, poolManager, options)
}
