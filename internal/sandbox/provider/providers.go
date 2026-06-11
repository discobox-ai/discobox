package provider

import (
	"github.com/obot-platform/disco2/internal/sandbox"
	"github.com/obot-platform/disco2/internal/sandbox/provider/digitalocean"
	"github.com/obot-platform/disco2/internal/sandbox/provider/dockervm"
	"github.com/obot-platform/disco2/internal/sandbox/vm"
)

func RegisterBuiltInSandboxProviderFactories(manager *sandbox.ProviderManager, workerStore vm.WorkerStore) {
	if manager == nil {
		return
	}
	manager.RegisterProviderDefinition(digitalocean.ProviderType, digitalocean.Definition)
	manager.RegisterFactory(digitalocean.ProviderType, digitalocean.FactoryWithWorkerStore(workerStore))
	manager.RegisterProviderConfigValidator(digitalocean.ProviderType, digitalocean.Validate)
	manager.RegisterProviderDefinition(dockervm.ProviderType, dockervm.Definition)
	manager.RegisterFactory(dockervm.ProviderType, dockervm.FactoryWithWorkerStore(workerStore))
	manager.RegisterProviderConfigValidator(dockervm.ProviderType, dockervm.Validate)
}
