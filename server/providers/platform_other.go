//go:build !windows && !darwin

package providers

import (
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/poolruntime"
)

// registerPlatformProviderFactories registers no additional providers. The
// platform-specific providers are wslc on Windows and vz on macOS; neither can
// start anywhere else.
func registerPlatformProviderFactories(*sandbox.ProviderManager, poolruntime.PoolManager, FactoryOptions) {
}
