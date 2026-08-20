package providers

import (
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/poolruntime"
	"github.com/obot-platform/discobox/server/providers/vz"
)

// registerPlatformProviderFactories registers the macOS-only vz provider, which
// runs one Virtualization.framework VM per pool. It is build-tagged for the
// same reason wslc is: the framework exists on macOS only, so no other platform
// should advertise a provider that cannot start there.
func registerPlatformProviderFactories(manager *sandbox.ProviderManager, poolManager poolruntime.PoolManager, options FactoryOptions) {
	manager.RegisterProviderDefinition(vz.ProviderType, vz.Definition())
	manager.RegisterFactory(vz.ProviderType, vz.FactoryWithPoolManager(poolManager, options.DevelopmentImageSync, controlPlaneStreams(options)))
	manager.RegisterProviderConfigValidator(vz.ProviderType, vz.Validate)
}

// controlPlaneStreams avoids handing the driver a typed-nil sink, which would
// satisfy the interface while panicking on use.
func controlPlaneStreams(options FactoryOptions) vz.StreamSink {
	if options.ControlPlaneStreams == nil {
		return nil
	}
	return options.ControlPlaneStreams
}
