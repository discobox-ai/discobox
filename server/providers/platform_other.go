//go:build !windows

package providers

import (
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/poolruntime"
)

// registerPlatformProviderFactories registers no additional providers. The only
// platform-specific provider today is wslc, which exists on Windows only.
func registerPlatformProviderFactories(*sandbox.ProviderManager, poolruntime.PoolManager, FactoryOptions) {
}
