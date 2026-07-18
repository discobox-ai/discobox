package dockerworker

import (
	"strconv"
	"strings"

	poolagent "github.com/obot-platform/discobox/pool-agent"
)

// BootEnv renders the pool-agent bootstrap contract as container environment
// variables so the in-container worker agent can register itself with the
// control plane after start.
func BootEnv(bootstrap poolagent.Bootstrap) map[string]string {
	env := map[string]string{
		poolagent.EnvControlPlaneURL: bootstrap.ControlPlaneURL,
		poolagent.EnvProjectID:       bootstrap.ProjectID,
		poolagent.EnvPoolID:          bootstrap.PoolID,
		poolagent.EnvBootstrapToken:  bootstrap.Token,
		poolagent.EnvControlPlaneKey: bootstrap.ControlPlaneKey,
	}
	if bootstrap.AgentPort > 0 {
		env[poolagent.EnvAgentPort] = strconv.Itoa(bootstrap.AgentPort)
	}
	if bootstrap.HostMountPrefix != "" {
		env[poolagent.EnvHostMountPrefix] = bootstrap.HostMountPrefix
	}
	for key, value := range env {
		if strings.TrimSpace(value) == "" {
			delete(env, key)
		}
	}
	return env
}
