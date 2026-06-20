package provider

import (
	"github.com/obot-platform/discobox/providers/sandbox/provider/digitalocean"
	"github.com/obot-platform/discobox/providers/sandbox/provider/docker"
	"github.com/obot-platform/discobox/providers/sandbox/vm"
	sandbox "github.com/obot-platform/discobox/sandboxprovider"
)

func RegisterBuiltInSandboxProviderFactories(manager *sandbox.ProviderManager, workerManager vm.WorkerManager) {
	if manager == nil {
		return
	}
	manager.RegisterProviderDefinition(digitalocean.ProviderType, digitalocean.Definition)
	manager.RegisterFactory(digitalocean.ProviderType, digitalocean.FactoryWithWorkerManager(workerManager))
	manager.RegisterProviderConfigValidator(digitalocean.ProviderType, digitalocean.Validate)
	manager.RegisterProviderDefinition(docker.ProviderType, docker.Definition)
	manager.RegisterFactory(docker.ProviderType, docker.FactoryWithWorkerManager(workerManager))
	manager.RegisterProviderConfigValidator(docker.ProviderType, docker.Validate)
}
