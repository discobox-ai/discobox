// Package workerbootstrap defines the shared worker boot metadata contract.
package workerbootstrap

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

const (
	EnvControlPlaneURL = "DISCOBOX_CONTROL_PLANE_URL"
	EnvProjectID       = "DISCOBOX_PROJECT_ID"
	EnvSandboxID       = "DISCOBOX_SANDBOX_ID"
	EnvWorkerID        = "DISCOBOX_WORKER_ID"
	EnvBootstrapToken  = "DISCOBOX_WORKER_BOOTSTRAP_TOKEN"
	EnvAgentPort       = "DISCOBOX_AGENT_PORT"
)

// Bootstrap is the VM boot contract used by the control plane, providers, and worker agent.
type Bootstrap struct {
	ControlPlaneURL string `json:"controlPlaneUrl,omitempty"`
	ProjectID       string `json:"projectId,omitempty"`
	SandboxID       string `json:"sandboxId,omitempty"`
	WorkerID        string `json:"workerId,omitempty"`
	Token           string `json:"token,omitempty"`
	AgentPort       int    `json:"agentPort,omitempty"`
}

// FromEnv builds Bootstrap from environment variables.
func FromEnv() Bootstrap {
	agentPort, _ := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvAgentPort)))
	return Bootstrap{
		ControlPlaneURL: strings.TrimSpace(os.Getenv(EnvControlPlaneURL)),
		ProjectID:       strings.TrimSpace(os.Getenv(EnvProjectID)),
		SandboxID:       strings.TrimSpace(os.Getenv(EnvSandboxID)),
		WorkerID:        strings.TrimSpace(os.Getenv(EnvWorkerID)),
		Token:           strings.TrimSpace(os.Getenv(EnvBootstrapToken)),
		AgentPort:       agentPort,
	}
}

// Validate checks the required worker bootstrap fields.
func (b Bootstrap) Validate() error {
	if strings.TrimSpace(b.ControlPlaneURL) == "" {
		return errors.New("control plane URL is required")
	}
	if strings.TrimSpace(b.WorkerID) == "" {
		return errors.New("worker ID is required")
	}
	if strings.TrimSpace(b.Token) == "" {
		return errors.New("worker bootstrap token is required")
	}
	return nil
}
