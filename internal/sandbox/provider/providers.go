package provider

import (
	"github.com/obot-platform/discobox/internal/sandbox"
	"github.com/obot-platform/discobox/internal/sandbox/provider/digitalocean"
	"github.com/obot-platform/discobox/internal/sandbox/provider/docker"
	"github.com/obot-platform/discobox/internal/sandbox/vm"
)

func RegisterBuiltInSandboxProviderFactories(manager *sandbox.ProviderManager, workerStore vm.WorkerStore) {
	if manager == nil {
		return
	}
	manager.RegisterProviderDefinition(digitalocean.ProviderType, digitalocean.Definition)
	manager.RegisterFactory(digitalocean.ProviderType, digitalocean.FactoryWithWorkerStore(workerStore))
	manager.RegisterProviderConfigValidator(digitalocean.ProviderType, digitalocean.Validate)
	manager.RegisterProviderDefinition(docker.ProviderType, docker.Definition)
	manager.RegisterFactory(docker.ProviderType, docker.FactoryWithWorkerStore(workerStore))
	manager.RegisterProviderConfigValidator(docker.ProviderType, docker.Validate)
}
