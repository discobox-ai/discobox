//go:build !windows && !darwin

package providers

import (
	"context"
	"runtime"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/libkrun"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
	"github.com/discobox-ai/discobox/version"
)

// registerPlatformProviderFactories registers no additional providers. The
// platform-specific providers are wslc on Windows and vz on macOS; neither can
// start anywhere else.
func registerPlatformProviderFactories(*sandbox.ProviderManager, poolruntime.PoolManager, FactoryOptions) {
}

// DefaultBootImages are the images the provider this server installs by default
// on this OS boots before it can run a pool (ADR 0113 §1). configured is the
// server's defaultProvider setting. On amd64 Linux, libkrun — a release build's
// default, or configured — boots its image (ADR 0148 §6); the host's Docker, a
// development build's default, boots nothing.
func DefaultBootImages(configured string) []string {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return nil
	}
	if configured == libkrun.ProviderType || (configured == "" && version.Released()) {
		return []string{libkrun.DefaultImage}
	}
	return nil
}

// ensurePlatformPrerequisites has nothing to check. There is no platform
// backend here: pools run on the portable providers, which are configured per
// instance and fail per instance, so a Docker daemon that is not running is one
// provider's problem rather than the whole server's — and a Linux server that
// cannot reach one at all is already refused by the harness check, which needs
// the same daemon to inspect an image.
func ensurePlatformPrerequisites(context.Context, FactoryOptions) error { return nil }
