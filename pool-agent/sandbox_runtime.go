package poolagent

import (
	"net/http"

	"github.com/obot-platform/discobox/pool-agent/sandboxruntime"
	poolserver "github.com/obot-platform/discobox/pool-agent/server"
)

type SandboxRuntime = sandboxruntime.Runtime
type DockerSandboxRuntime = sandboxruntime.DockerSandboxRuntime
type DockerSandboxRuntimeConfig = sandboxruntime.DockerSandboxRuntimeConfig
type MemorySandboxRuntime = sandboxruntime.MemorySandboxRuntime

func NewDockerSandboxRuntime(cfg DockerSandboxRuntimeConfig) (*DockerSandboxRuntime, error) {
	return sandboxruntime.NewDockerSandboxRuntime(cfg)
}

func NewMemorySandboxRuntime() *MemorySandboxRuntime {
	return sandboxruntime.NewMemorySandboxRuntime()
}

func NewSandboxHandler(bootstrap Bootstrap, runtime SandboxRuntime) http.Handler {
	router, _ := poolserver.NewRouter(poolserver.Config{
		Identity: poolserver.Identity{
			ProjectID: bootstrap.ProjectID,
			SandboxID: bootstrap.SandboxID,
			PoolID:    bootstrap.PoolID,
		},
		Runtime:               runtime,
		ControlPlanePublicKey: bootstrap.ControlPlaneKey,
	})
	return router
}
