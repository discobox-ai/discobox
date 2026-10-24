package dockerworker

import (
	"encoding/json"
	"strings"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// RuntimeState is the engine-owned pool runtime state persisted on the
// pool row. InstanceID identifies the driver's VM (empty for the local
// driver); ContainerID identifies the pool's container in that VM's Docker
// daemon; Box is what the engine placed in it, the server's own record of it
// (ADR 26-10-24-953 §5), which outlives this process with the pool's row.
type RuntimeState struct {
	InstanceID  string        `json:"instanceId,omitempty"`
	ContainerID string        `json:"containerId,omitempty"`
	Box         poolBoxRecord `json:"box,omitzero"`
}

// DecodeRuntimeState parses persisted pool runtime state. It returns
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
