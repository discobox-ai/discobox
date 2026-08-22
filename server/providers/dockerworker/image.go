package dockerworker

import (
	"os"
	"strings"
)

// DefaultPoolImage is the default pool-agent container image launched by the
// engine on every backend. The name is the one the rest of the tree uses for
// this image — the Taskfile builds discobox-pool-agent:local and the image
// watcher tags discobox-pool-agent:dev-* — rather than a third spelling of it.
//
// A release overwrites this at link time with the image built for that release,
// so a released binary pulls the pool agent it was cut alongside rather than
// whatever :latest happens to be. It is a var, not a const, for exactly that
// reason. A development build keeps :latest and local development overrides it
// through PoolImageEnv.
var DefaultPoolImage = "ghcr.io/discobox-ai/discobox-pool-agent:latest"

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
