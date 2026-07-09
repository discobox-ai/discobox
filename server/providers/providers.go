package providers

import (
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/digitalocean"
	"github.com/obot-platform/discobox/server/providers/docker"
	"github.com/obot-platform/discobox/server/providers/execvm"
	"github.com/obot-platform/discobox/server/providers/workerpool"
)

func RegisterBuiltInSandboxProviderFactories(manager *sandbox.ProviderManager, workerManager workerpool.WorkerManager) {
	if manager == nil {
		return
	}
	manager.RegisterProviderDefinition(digitalocean.ProviderType, digitalocean.Definition())
	manager.RegisterFactory(digitalocean.ProviderType, digitalocean.FactoryWithWorkerManager(workerManager))
	manager.RegisterProviderConfigValidator(digitalocean.ProviderType, digitalocean.Validate)
	manager.RegisterProviderDefinition(docker.ProviderType, docker.Definition())
	manager.RegisterFactory(docker.ProviderType, docker.FactoryWithWorkerManager(workerManager))
	manager.RegisterProviderConfigValidator(docker.ProviderType, docker.Validate)
	manager.RegisterProviderDefinition(execvm.ProviderType, execvm.Definition())
	manager.RegisterFactory(execvm.ProviderType, execvm.FactoryWithWorkerManager(workerManager))
	manager.RegisterProviderConfigValidator(execvm.ProviderType, execvm.Validate)
}
