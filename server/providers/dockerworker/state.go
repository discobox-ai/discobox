package dockerworker

import (
	"encoding/json"
	"strings"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// RuntimeState is the engine-owned worker runtime state persisted on the
// worker row. InstanceID identifies the driver's VM (empty for the local
// driver); ContainerID identifies the pool-agent container in that VM's
// Docker daemon.
type RuntimeState struct {
	InstanceID  string `json:"instanceId,omitempty"`
	ContainerID string `json:"containerId,omitempty"`
}

// DecodeRuntimeState parses persisted worker runtime state. It returns
// sandbox.ErrNotFound when the state is empty or carries no runtime identity.
func DecodeRuntimeState(data []byte) (RuntimeState, error) {
	if len(data) == 0 {
		return RuntimeState{}, sandbox.ErrNotFound
	}
	var state RuntimeState
	if err := json.Unmarshal(data, &state); err != nil {
		return RuntimeState{}, err
	}
	if strings.TrimSpace(state.InstanceID) == "" && strings.TrimSpace(state.ContainerID) == "" {
		return RuntimeState{}, sandbox.ErrNotFound
	}
	return state, nil
}

func encodeRuntimeState(state RuntimeState) ([]byte, error) {
	return json.Marshal(state)
}
