package providers

import (
	"context"
	"runtime"
	"slices"

	"github.com/discobox-ai/discobox/releasemanifest"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport/carrierhub"
	"github.com/discobox-ai/discobox/server/providers/digitalocean"
	"github.com/discobox-ai/discobox/server/providers/docker"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/execvm"
	"github.com/discobox-ai/discobox/server/providers/libkrun"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

type FactoryOptions struct {
	DevelopmentImageSync *dockerworker.DevelopmentImageSynchronizer
	// ControlPlaneStreams receives connections that a pool guest opens toward
	// the control plane. Only backends whose transport cannot be dialed inward
	// use it; the server serves its ordinary handler over whatever arrives.
	ControlPlaneStreams *carrierhub.Hub
	// ListenEndpoints are the endpoints the control plane listens on. A backend
	// whose agent dials inward picks its address from them.
	ListenEndpoints []string
	// ServerDefaults are the provider settings the server configures rather
	// than any one provider instance.
	ServerDefaults dockerworker.ServerDefaults
	// WSLCCommand overrides the WSL Containers program the Windows host is
	// checked for. Empty looks for the component's own name.
	WSLCCommand string
}

func RegisterBuiltInSandboxProviderFactories(manager *sandbox.ProviderManager, poolManager poolruntime.PoolManager, options FactoryOptions) {
	if manager == nil {
		return
	}
	manager.RegisterProviderDefinition(digitalocean.ProviderType, digitalocean.Definition())
	manager.RegisterFactory(digitalocean.ProviderType, digitalocean.FactoryWithPoolManager(poolManager, options.DevelopmentImageSync, options.ServerDefaults))
	manager.RegisterProviderConfigValidator(digitalocean.ProviderType, digitalocean.Validate)
	manager.RegisterProviderDefinition(docker.ProviderType, docker.Definition())
	manager.RegisterFactory(docker.ProviderType, docker.FactoryWithPoolManager(poolManager, options.DevelopmentImageSync, options.ListenEndpoints, options.ServerDefaults))
	manager.RegisterProviderConfigValidator(docker.ProviderType, docker.Validate)
	manager.RegisterProviderDefinition(execvm.ProviderType, execvm.Definition())
	manager.RegisterFactory(execvm.ProviderType, execvm.FactoryWithPoolManager(poolManager, options.DevelopmentImageSync, options.ServerDefaults))
	manager.RegisterProviderConfigValidator(execvm.ProviderType, execvm.Validate)
	manager.RegisterProviderDefinition(libkrun.ProviderType, libkrun.Definition())
	manager.RegisterFactory(libkrun.ProviderType, libkrun.FactoryWithPoolManager(poolManager, options.DevelopmentImageSync, options.ServerDefaults))
	manager.RegisterProviderConfigValidator(libkrun.ProviderType, libkrun.Validate)
	registerPlatformProviderFactories(manager, poolManager, options)
}

// EnsurePlatformPrerequisites reports whether this host has what the platform's
// own pool backend needs before anything asks it for a pool.
//
// Only a platform backend can fail this way. The portable providers are
// configured per instance and fail per instance — a Docker daemon that is not
// running is one provider's problem — but the platform backend is the default
// on the host it belongs to (`sandbox.PlatformDefaultProvider`), so a missing
// prerequisite is every pool on the machine, and the server refuses to serve
// rather than making each sandbox create discover it.
func EnsurePlatformPrerequisites(ctx context.Context, options FactoryOptions) error {
	return ensurePlatformPrerequisites(ctx, options)
}

// CheckDefaultProvider reports whether a first start can install providerType
// as this host's default, and a *sandbox.ProviderUnavailableError when it
// cannot (ADR 26-09-23-473 §2).
//
// Only libkrun has anything to check. It is the default that needs the host to
// have something — KVM, and a runtime that loads — and a missing one is the
// difference between the VM boundary a user was promised and none at all. The
// platform backends are checked for the whole server by
// EnsurePlatformPrerequisites, and the portable providers fail per instance.
func CheckDefaultProvider(ctx context.Context, providerType string, options FactoryOptions) error {
	if providerType == libkrun.ProviderType {
		return libkrun.CheckHost(ctx, options.ServerDefaults)
	}
	return nil
}

// DefaultProviderType is the provider a first start installs on this host
// (ADR 26-09-23-473 §1), and the one place that rule is decided: the service
// installs it, and the boot and release images staged for a first start
// follow it. Each OS has its VM backend, and Linux's is libkrun in a release
// build. A development build on Linux installs the host's Docker instead: the
// `task dev` loop converges the images it builds onto that daemon, and a
// contributor's machine should not need KVM to run the product. configured is
// the server's defaultProvider setting, which decides it on Linux, the only OS
// with two to choose from.
func DefaultProviderType(configured string, released bool) string {
	switch runtime.GOOS {
	case "linux":
		if configured != "" {
			return configured
		}
		if released {
			return libkrun.ProviderType
		}
		return docker.ProviderType
	case "darwin":
		return "vz"
	case "windows":
		return "wslc"
	default:
		return "unsupported"
	}
}

// ReleaseImages are the images of a release manifest a first start on this
// host stages, for a first start that will install providerType
// (DefaultProviderType). The libkrun image is left out unless libkrun is what
// it installs: a host told to install Docker boots no libkrun VM, and staging
// one is hundreds of megabytes downloaded for nothing.
func ReleaseImages(images releasemanifest.Images, providerType string) []string {
	refs := images.References(runtime.GOOS, runtime.GOARCH)
	if providerType == libkrun.ProviderType || images.Libkrun == "" {
		return refs
	}
	return slices.DeleteFunc(refs, func(ref string) bool { return ref == images.Libkrun })
}
