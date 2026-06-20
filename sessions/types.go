package sessions

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

type Agent struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Command     []string `json:"command"`
	Description string   `json:"description,omitempty"`
}

type Config struct {
	Agents []Agent `json:"agents"`
}

type Session struct {
	ID        string     `json:"id"`
	AgentID   string     `json:"agentId"`
	Command   []string   `json:"command"`
	Workdir   string     `json:"workdir"`
	PID       int        `json:"pid,omitempty"`
	Running   bool       `json:"running"`
	ExitCode  *int       `json:"exitCode,omitempty"`
	Error     string     `json:"error,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	ExitedAt  *time.Time `json:"exitedAt,omitempty"`
}

type PingResponse struct {
	SessionID string `json:"sessionId"`
	RepoRoot  string `json:"repoRoot"`
	Version   int64  `json:"version"`
}

type StatusResponse struct {
	SessionID string    `json:"sessionId"`
	RepoRoot  string    `json:"repoRoot"`
	Version   int64     `json:"version"`
	Sessions  []Session `json:"sessions"`
}

type AgentsResponse struct {
	Agents []Agent `json:"agents"`
}

type SessionsResponse struct {
	Sessions []Session `json:"sessions"`
}

type CreateRequest struct {
	AgentID string   `json:"agentId"`
	Args    []string `json:"args,omitempty"`
	Workdir string   `json:"workdir,omitempty"`
	Cols    uint16   `json:"cols,omitempty"`
	Rows    uint16   `json:"rows,omitempty"`
}

type CreateResponse struct {
	Session Session `json:"session"`
}

type ResizeRequest struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

type SignalRequest struct {
	Signal string `json:"signal"`
}

type ShutdownResponse struct {
	Shutdown bool `json:"shutdown"`
}

const (
	FrameOutput byte = 1
	FrameInput  byte = 2
	FrameResize byte = 3
	FrameSignal byte = 4
	FrameError  byte = 5
)

const maxFramePayload = 16 * 1024 * 1024

type Frame struct {
	Type    byte
	Payload []byte
}

func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > maxFramePayload {
		return fmt.Errorf("frame payload too large: %d", len(payload))
	}
	var header [5]byte
	header[0] = typ
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

func ReadFrame(r io.Reader) (Frame, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size > maxFramePayload {
		return Frame{}, fmt.Errorf("frame payload too large: %d", size)
	}
	payload := make([]byte, int(size))
	if size > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return Frame{}, err
		}
	}
	return Frame{Type: header[0], Payload: payload}, nil
}

func EncodeResize(cols, rows uint16) ([]byte, error) {
	return json.Marshal(ResizeRequest{Cols: cols, Rows: rows})
}

func DecodeResize(payload []byte) (ResizeRequest, error) {
	var req ResizeRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return ResizeRequest{}, err
	}
	if req.Cols == 0 || req.Rows == 0 {
		return ResizeRequest{}, fmt.Errorf("rows and cols are required")
	}
	return req, nil
}

func DefaultAgents() []Agent {
	return []Agent{
		{ID: "codex", Name: "Codex", Command: []string{"codex"}},
		{ID: "claude-code", Name: "Claude Code", Command: []string{"claude"}},
		{ID: "opencode", Name: "OpenCode", Command: []string{"opencode"}},
	}
}

func MergeConfig(defaults []Agent, cfg Config) ([]Agent, error) {
	agents := make([]Agent, 0, len(defaults))
	byID := map[string]int{}
	for _, agent := range defaults {
		if err := validateAgent(agent); err != nil {
			return nil, err
		}
		byID[agent.ID] = len(agents)
		agents = append(agents, agent)
	}
	for _, override := range cfg.Agents {
		if err := validateAgent(override); err != nil {
			return nil, err
		}
		idx, ok := byID[override.ID]
		if !ok {
			return nil, fmt.Errorf("unsupported agent %q", override.ID)
		}
		agents[idx] = override
	}
	return agents, nil
}

func AgentByID(agents []Agent, id string) (Agent, bool) {
	for _, agent := range agents {
		if agent.ID == id {
			return agent, true
		}
	}
	return Agent{}, false
}

func validateAgent(agent Agent) error {
	if strings.TrimSpace(agent.ID) == "" {
		return fmt.Errorf("agent id is required")
	}
	if len(agent.Command) == 0 || strings.TrimSpace(agent.Command[0]) == "" {
		return fmt.Errorf("agent %q command is required", agent.ID)
	}
	return nil
}
