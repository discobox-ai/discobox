package providers

import (
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
	"github.com/discobox-ai/discobox/server/providers/wslc"
)

// registerPlatformProviderFactories registers the Windows-only wslc provider,
// which runs one WSL Containers VM per pool. It is build-tagged rather than
// registered unconditionally so the Linux server does not advertise a provider
// that cannot start there.
func registerPlatformProviderFactories(manager *sandbox.ProviderManager, poolManager poolruntime.PoolManager, options FactoryOptions) {
	manager.RegisterProviderDefinition(wslc.ProviderType, wslc.Definition())
	manager.RegisterFactory(wslc.ProviderType, wslc.FactoryWithPoolManager(poolManager, options.DevelopmentImageSync, controlPlaneStreams(options)))
	manager.RegisterProviderConfigValidator(wslc.ProviderType, wslc.Validate)
}

// controlPlaneStreams avoids handing the driver a typed-nil sink, which would
// satisfy the interface while panicking on use.
func controlPlaneStreams(options FactoryOptions) wslc.StreamSink {
	if options.ControlPlaneStreams == nil {
		return nil
	}
	return options.ControlPlaneStreams
}
