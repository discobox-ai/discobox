package poolagent

import (
	"net/http"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	poolserver "github.com/discobox-ai/discobox/pool-agent/server"
)

type SandboxRuntime = sandboxruntime.Runtime
type MemorySandboxRuntime = sandboxruntime.MemorySandboxRuntime

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
