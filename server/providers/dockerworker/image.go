package dockerworker

import (
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/imagecache"
	"github.com/discobox-ai/discobox/pkg/releasemanifest"
)

// DefaultSandboxBaseImage is the Docker driver's sandbox base image: Debian,
// systemd, Docker and a login user, built from sandbox-image/Dockerfile, which
// every sandbox container and a Docker pool's own container run (ADR
// 26-10-16-423 §10, ADR 26-10-24-953 §2). A release overwrites it at link
// time with the image built for that release, so a released binary pulls the
// image it was cut alongside rather than whatever :latest happens to be; it is
// a var, not a const, for exactly that reason. A provider's
// `sandboxBaseImage` setting and the server's `dockerSandboxBaseImage`
// override it (ADR 0096 §5, configuration file).
var DefaultSandboxBaseImage = "ghcr.io/discobox-ai/discobox-sandbox-base:latest"

// DefaultPoolAgentBundle is the pool bundle, a pool's box's one bundle (ADR
// 26-10-24-953 §2), resolved as DefaultSandboxBaseImage is but for a
// provider's setting: a release overwrites it at link time and names it in
// its manifest (Bundles.PoolAgent), and the server's `poolAgentBundle`
// overrides it. A var for the same reason. A development server is handed the
// watcher's build through that setting.
var DefaultPoolAgentBundle = "ghcr.io/discobox-ai/discobox-pool-agent-bundle:latest"

// ServerDefaults are the provider settings that belong to the server rather
// than to any one provider instance (ADR 0096 §5, configuration file). They arrive as a value
// because they are configuration, and configuration is the server's to hold —
// a provider that read them from the environment would be reading a file it
// does not own.
type ServerDefaults struct {
	// Release pins the complete runtime image set when explicitly selected.
	Release *releasemanifest.Manifest
	// SandboxBaseImage overrides the sandbox base image, for a provider whose
	// own configuration names none.
	SandboxBaseImage string
	// PoolAgentBundle overrides the pool bundle; no provider has a setting of
	// its own for it.
	PoolAgentBundle string
	// AgentBundle overrides the agent bundle, which every box is placed with
	// (ADR 26-10-24-953 §2); no provider has a setting of its own for it.
	AgentBundle string
	// ImageRetention is how long an unused Discobox image is kept. Zero means
	// unconfigured, which is not the same as zero: see Config.ImageRetention.
	ImageRetention time.Duration
	// ImageCache is the image cache the CLI that launched this server stages
	// into, when the configuration names one (ADR 0113). A provider whose
	// daemon is on this machine hands it to its engine.
	ImageCache *imagecache.Layout
}

// EffectiveSandboxBaseImage resolves the sandbox base image: the release
// manifest when one is selected, then the provider's configuration, the
// server-wide override, and the compiled-in default.
//
// The override arrives as an argument rather than being read from the
// environment here: it is server configuration, and the server is what holds
// configuration (ADR 0096 §5, configuration file).
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

// EffectivePoolAgentBundle resolves the pool bundle as
// EffectiveSandboxBaseImage resolves the sandbox base image, but for a
// provider's own setting, which no provider has: the release manifest when
// one is selected, then the server-wide override, and the compiled-in
// default.
func EffectivePoolAgentBundle(defaults ServerDefaults) string {
	if defaults.Release != nil {
		return defaults.Release.Bundles.PoolAgent
	}
	if override := strings.TrimSpace(defaults.PoolAgentBundle); override != "" {
		return override
	}
	return DefaultPoolAgentBundle
}

// EffectiveAgentBundle resolves the agent bundle: the release manifest when
// one is selected, else the server's setting, which the server's
// configuration defaults to the one this build ships, as it does the
// platform bundle's. None is a server that names no agent.
func EffectiveAgentBundle(defaults ServerDefaults) string {
	if defaults.Release != nil {
		return defaults.Release.Bundles.Agent
	}
	return strings.TrimSpace(defaults.AgentBundle)
}
