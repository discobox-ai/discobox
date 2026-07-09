package dockerworker

import (
	"strconv"
	"strings"

	workeragent "github.com/obot-platform/discobox/worker-agent"
)

// BootEnv renders the worker-agent bootstrap contract as container environment
// variables so the in-container worker agent can register itself with the
// control plane after start.
func BootEnv(bootstrap workeragent.Bootstrap) map[string]string {
	env := map[string]string{
		workeragent.EnvControlPlaneURL: bootstrap.ControlPlaneURL,
		workeragent.EnvProjectID:       bootstrap.ProjectID,
		workeragent.EnvWorkerID:        bootstrap.WorkerID,
		workeragent.EnvBootstrapToken:  bootstrap.Token,
		workeragent.EnvControlPlaneKey: bootstrap.ControlPlaneKey,
	}
	if bootstrap.AgentPort > 0 {
		env[workeragent.EnvAgentPort] = strconv.Itoa(bootstrap.AgentPort)
	}
	if bootstrap.HostMountPrefix != "" {
		env[workeragent.EnvHostMountPrefix] = bootstrap.HostMountPrefix
	}
	for key, value := range env {
		if strings.TrimSpace(value) == "" {
			delete(env, key)
		}
	}
	return env
}
