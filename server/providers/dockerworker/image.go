package dockerworker

import (
	"os"
	"strings"
)

// DefaultPoolImage is the default pool-agent container image launched by the
// engine on every backend.
const DefaultPoolImage = "ghcr.io/obot-platform/discobox-systemd:latest"

// PoolImageEnv globally overrides the default pool-agent image, primarily for
// local development against freshly built images.
const PoolImageEnv = "DISCOBOX_DOCKER_POOL_IMAGE"

// EffectivePoolImage resolves the pool-agent image from provider
// configuration, the global override, or the static default.
func EffectivePoolImage(image string) string {
	if image = strings.TrimSpace(image); image != "" {
		return image
	}
	if value := strings.TrimSpace(os.Getenv(PoolImageEnv)); value != "" {
		return value
	}
	return DefaultPoolImage
}

// PoolImageSource reports where the effective pool image came from.
func PoolImageSource(image string) string {
	if strings.TrimSpace(image) != "" {
		return "provider"
	}
	if strings.TrimSpace(os.Getenv(PoolImageEnv)) != "" {
		return "global"
	}
	return "default"
}
