package providers

import (
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/digitalocean"
	"github.com/obot-platform/discobox/server/providers/docker"
	"github.com/obot-platform/discobox/server/providers/execvm"
	"github.com/obot-platform/discobox/server/providers/poolruntime"
)

func RegisterBuiltInSandboxProviderFactories(manager *sandbox.ProviderManager, poolManager poolruntime.PoolManager) {
	if manager == nil {
		return
	}
	manager.RegisterProviderDefinition(digitalocean.ProviderType, digitalocean.Definition())
	manager.RegisterFactory(digitalocean.ProviderType, digitalocean.FactoryWithPoolManager(poolManager))
	manager.RegisterProviderConfigValidator(digitalocean.ProviderType, digitalocean.Validate)
	manager.RegisterProviderDefinition(docker.ProviderType, docker.Definition())
	manager.RegisterFactory(docker.ProviderType, docker.FactoryWithPoolManager(poolManager))
	manager.RegisterProviderConfigValidator(docker.ProviderType, docker.Validate)
	manager.RegisterProviderDefinition(execvm.ProviderType, execvm.Definition())
	manager.RegisterFactory(execvm.ProviderType, execvm.FactoryWithPoolManager(poolManager))
	manager.RegisterProviderConfigValidator(execvm.ProviderType, execvm.Validate)
}
