//go:build !windows && !darwin

package providers

import (
	"context"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

// registerPlatformProviderFactories registers no additional providers. The
// platform-specific providers are wslc on Windows and vz on macOS; neither can
// start anywhere else.
func registerPlatformProviderFactories(*sandbox.ProviderManager, poolruntime.PoolManager, FactoryOptions) {
}

// ensurePlatformPrerequisites has nothing to check. There is no platform
// backend here: pools run on the portable providers, which are configured per
// instance and fail per instance, so a Docker daemon that is not running is one
// provider's problem rather than the whole server's — and a Linux server that
// cannot reach one at all is already refused by the harness check, which needs
// the same daemon to inspect an image.
func ensurePlatformPrerequisites(context.Context) error { return nil }
