package workeragent

import (
	"net/http"

	"github.com/obot-platform/discobox/worker-agent/workeragent/sandboxruntime"
	workerserver "github.com/obot-platform/discobox/worker-agent/workeragent/server"
)

type SandboxRuntime = sandboxruntime.Runtime
type DockerSandboxRuntime = sandboxruntime.DockerSandboxRuntime
type MemorySandboxRuntime = sandboxruntime.MemorySandboxRuntime

func NewDockerSandboxRuntime(projectID, workerID string) (*DockerSandboxRuntime, error) {
	return sandboxruntime.NewDockerSandboxRuntime(projectID, workerID)
}

func NewMemorySandboxRuntime() *MemorySandboxRuntime {
	return sandboxruntime.NewMemorySandboxRuntime()
}

func NewSandboxHandler(bootstrap Bootstrap, runtime SandboxRuntime, authTokens ...string) http.Handler {
	router, _ := workerserver.NewRouter(workerserver.Config{
		Identity: workerserver.Identity{
			ProjectID: bootstrap.ProjectID,
			SandboxID: bootstrap.SandboxID,
			WorkerID:  bootstrap.WorkerID,
		},
		Runtime:    runtime,
		AuthTokens: authTokens,
	})
	return router
}
