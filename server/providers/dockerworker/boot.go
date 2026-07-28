package dockerworker

import (
	"strings"

	poolagent "github.com/obot-platform/discobox/pool-agent"
)

// BootEnv renders the pool-agent bootstrap contract as container environment
// variables so the in-container worker agent can register itself with the
// control plane after start.
//
// Both directions are a single URL: the scheme carries the transport, so adding
// a backend never adds a variable here.
func BootEnv(bootstrap poolagent.Bootstrap) map[string]string {
	env := map[string]string{
		poolagent.EnvControlPlaneURL: bootstrap.ControlPlaneURL,
		poolagent.EnvProjectID:       bootstrap.ProjectID,
		poolagent.EnvPoolID:          bootstrap.PoolID,
		poolagent.EnvBootstrapToken:  bootstrap.Token,
		poolagent.EnvControlPlaneKey: bootstrap.ControlPlaneKey,
		poolagent.EnvAgentListenURL:  bootstrap.AgentListenURL,
		poolagent.EnvHostMountPrefix: bootstrap.HostMountPrefix,
		poolagent.EnvHostStateRoot:   bootstrap.HostStateRoot,
	}
	for key, value := range env {
		if strings.TrimSpace(value) == "" {
			delete(env, key)
		}
	}
	return env
}
