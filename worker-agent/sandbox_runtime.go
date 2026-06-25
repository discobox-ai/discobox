package workeragent

import (
	"net/http"

	"github.com/obot-platform/discobox/worker-agent/sandboxruntime"
	workerserver "github.com/obot-platform/discobox/worker-agent/server"
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
	router, _ := workerserver.NewRouter(workerserver.Config{
		Identity: workerserver.Identity{
			ProjectID: bootstrap.ProjectID,
			SandboxID: bootstrap.SandboxID,
			WorkerID:  bootstrap.WorkerID,
		},
		Runtime:               runtime,
		ControlPlanePublicKey: bootstrap.ControlPlaneKey,
	})
	return router
}
