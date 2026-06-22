package provider

import (
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/sandbox/provider/digitalocean"
	"github.com/obot-platform/discobox/server/providers/sandbox/provider/docker"
	"github.com/obot-platform/discobox/server/providers/sandbox/workerpool"
)

func RegisterBuiltInSandboxProviderFactories(manager *sandbox.ProviderManager, workerManager workerpool.WorkerManager) {
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
