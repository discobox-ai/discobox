package dockercache

import "context"

// SetBridgeConfigPath points the forwarder-config check at a fixture.
func SetBridgeConfigPath(path string) { bridgeConfigPath = path }

// DockerCLI is the docker binary the shim runs, for tests asserting on argv.
func DockerCLI() string { return dockerCLI }

// DefaultDockerCLI is the binary production runs, for a test to restore.
const DefaultDockerCLI = realDocker

// SetDockerCLI points every docker subprocess at a stub.
func SetDockerCLI(path string) { dockerCLI = path }

// BuildViaRegistry runs the push/pull/tag sequence a rewritten build takes.
func BuildViaRegistry(ctx context.Context, a Args) int { return buildViaRegistry(ctx, a) }
