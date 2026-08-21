package poolagent

import (
	"net/http"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	poolserver "github.com/discobox-ai/discobox/pool-agent/server"
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
			PoolID:    bootstrap.PoolID,
		},
		Runtime:               runtime,
		ControlPlanePublicKey: bootstrap.ControlPlaneKey,
	})
	return router
}
