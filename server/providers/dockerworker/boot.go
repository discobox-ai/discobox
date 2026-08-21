package dockerworker

import (
	"strings"

	poolagent "github.com/discobox-ai/discobox/pool-agent"
	"github.com/discobox-ai/discobox/pool-agent/imagereap"
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

// poolContainerEnv is the pool-agent's whole environment: the bootstrap
// contract, plus engine-level policy the agent applies to its own Docker daemon.
//
// The policy is kept out of Bootstrap deliberately. Bootstrap is identity and
// transport — "a backend is expressed entirely in the URLs it renders" — and
// image retention is neither. Policy that was never configured is omitted rather
// than defaulted, so a deployment that sets nothing produces the same container
// configuration, and the same revision, as before this existed.
func (e *Engine) poolContainerEnv(bootstrap poolagent.Bootstrap) map[string]string {
	env := BootEnv(bootstrap)
	if e.cfg.ImageRetention > 0 {
		env[imagereap.RetentionEnv] = e.cfg.ImageRetention.String()
	}
	return env
}
