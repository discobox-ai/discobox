package dockerworker

import (
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/imagecache"
	"github.com/discobox-ai/discobox/pkg/releasemanifest"
)

// DefaultPoolImage is the default pool-agent container image launched by the
// engine on every backend. The name is the one the rest of the tree uses for
// this image — the Taskfile builds discobox-pool-agent:local and the image
// watcher tags discobox-pool-agent:dev-* — rather than a third spelling of it.
//
// A release overwrites this at link time with the image built for that release,
// so a released binary pulls the pool agent it was cut alongside rather than
// whatever :latest happens to be. It is a var, not a const, for exactly that
// reason. A development build keeps :latest, and local development overrides
// it with the server's `dockerPoolImage` setting — DISCOBOX_DOCKER_POOL_IMAGE
// in the environment — which arrives here as ServerDefaults.PoolImage
// (ADR 0096 §5, configuration file).
var DefaultPoolImage = "ghcr.io/discobox-ai/discobox-pool-agent:latest"

// DefaultSandboxBaseImage is the Docker driver's sandbox base image: Debian,
// systemd, Docker and a login user, built from sandbox-image/Dockerfile, which
// the sandbox images are built FROM and a pool preloads (ADR 26-10-16-423
// §10). It is
// resolved as DefaultPoolImage is — a release overwrites it at link time, a
// provider's `sandboxBaseImage` setting and the server's
// `dockerSandboxBaseImage` override it — and is a var for the same reason.
var DefaultSandboxBaseImage = "ghcr.io/discobox-ai/discobox-sandbox-base:latest"

// ServerDefaults are the provider settings that belong to the server rather
// than to any one provider instance (ADR 0096 §5, configuration file). They arrive as a value
// because they are configuration, and configuration is the server's to hold —
// a provider that read them from the environment would be reading a file it
// does not own.
type ServerDefaults struct {
	// Release pins the complete runtime image set when explicitly selected.
	Release *releasemanifest.Manifest
	// PoolImage overrides the pool-agent image, for a provider whose own
	// configuration names none.
	PoolImage string
	// SandboxBaseImage overrides the sandbox base image, for a provider whose
	// own configuration names none.
	SandboxBaseImage string
	// ImageRetention is how long an unused Discobox image is kept. Zero means
	// unconfigured, which is not the same as zero: see Config.ImageRetention.
	ImageRetention time.Duration
	// ImageCache is the image cache the CLI that launched this server stages
	// into, when the configuration names one (ADR 0113). A provider whose
	// daemon is on this machine hands it to its engine.
	ImageCache *imagecache.Layout
}

// EffectivePoolImage resolves the pool-agent image from provider
// configuration, the server-wide override, or the static default.
//
// The override arrives as an argument rather than being read from the
// environment here: it is server configuration, and the server is what holds
// configuration (ADR 0096 §5, configuration file).
func EffectivePoolImage(image string, defaults ServerDefaults) string {
	if defaults.Release != nil {
		return defaults.Release.Images.PoolAgent
	}
	override := defaults.PoolImage
	if image = strings.TrimSpace(image); image != "" {
		return image
	}
	if override = strings.TrimSpace(override); override != "" {
		return override
	}
	return DefaultPoolImage
}

// EffectiveSandboxBaseImage resolves the sandbox base image as
// EffectivePoolImage resolves the pool-agent image: the release manifest when
// one is selected, then the provider's configuration, the server-wide
// override, and the compiled-in default.
func EffectiveSandboxBaseImage(image string, defaults ServerDefaults) string {
	if defaults.Release != nil {
		return defaults.Release.Images.SandboxBase
	}
	if image = strings.TrimSpace(image); image != "" {
		return image
	}
	if override := strings.TrimSpace(defaults.SandboxBaseImage); override != "" {
		return override
	}
	return DefaultSandboxBaseImage
}

// PoolImageSource reports where the effective pool image came from.
func PoolImageSource(image string, defaults ServerDefaults) string {
	if defaults.Release != nil {
		return "release manifest"
	}
	override := defaults.PoolImage
	if strings.TrimSpace(image) != "" {
		return "provider"
	}
	if strings.TrimSpace(override) != "" {
		return "server"
	}
	return "default"
}
