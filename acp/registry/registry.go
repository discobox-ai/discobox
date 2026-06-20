// Package registry models and loads ACP registry metadata.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"time"
)

const DefaultURL = "https://cdn.agentclientprotocol.com/registry/v1/latest/registry.json"

var supportedAgents = map[string]SupportedAgent{
	"codex-acp": {
		ID:          "codex-acp",
		Aliases:     []string{"codex"},
		Description: "Codex CLI ACP adapter",
	},
}

// SupportedAgent describes an ACP implementation Discobox intentionally exposes.
type SupportedAgent struct {
	ID          string   `json:"id"`
	Aliases     []string `json:"aliases,omitempty"`
	Description string   `json:"description,omitempty"`
}

// Registry is the official ACP registry index shape.
type Registry struct {
	Version string  `json:"version"`
	Agents  []Agent `json:"agents"`
}

// Agent is one ACP registry agent entry.
type Agent struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	Description  string       `json:"description"`
	Repository   string       `json:"repository,omitempty"`
	Website      string       `json:"website,omitempty"`
	Authors      []string     `json:"authors,omitempty"`
	License      string       `json:"license,omitempty"`
	Icon         string       `json:"icon,omitempty"`
	Distribution Distribution `json:"distribution"`
}

// Distribution contains the supported ACP registry launch mechanisms.
type Distribution struct {
	Binary map[string]BinaryTarget `json:"binary,omitempty"`
	NPX    *PackageTarget          `json:"npx,omitempty"`
	UVX    *PackageTarget          `json:"uvx,omitempty"`
}

// BinaryTarget is a platform-specific binary archive distribution.
type BinaryTarget struct {
	Archive string            `json:"archive"`
	Cmd     string            `json:"cmd"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// PackageTarget is an npx or uvx package distribution.
type PackageTarget struct {
	Package string            `json:"package"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// ListSupported returns the explicit supported agent list.
func ListSupported() []SupportedAgent {
	out := make([]SupportedAgent, 0, len(supportedAgents))
	for _, agent := range supportedAgents {
		out = append(out, agent)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ResolveSupportedID maps an id or alias to a supported registry id.
func ResolveSupportedID(id string) (string, error) {
	id = strings.TrimSpace(strings.ToLower(id))
	if id == "" {
		return "", errors.New("agent id is required")
	}
	for _, agent := range supportedAgents {
		if id == agent.ID {
			return agent.ID, nil
		}
		for _, alias := range agent.Aliases {
			if id == alias {
				return agent.ID, nil
			}
		}
	}
	return "", fmt.Errorf("unsupported ACP agent %q", id)
}

// Fetch loads a registry index.
func Fetch(ctx context.Context, url string) (*Registry, error) {
	if url == "" {
		url = DefaultURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch registry: unexpected status %s", resp.Status)
	}
	var reg Registry
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		return nil, fmt.Errorf("decode registry: %w", err)
	}
	return &reg, nil
}

// FindAgent returns one supported agent from the registry.
func (r *Registry) FindAgent(id string) (Agent, error) {
	resolved, err := ResolveSupportedID(id)
	if err != nil {
		return Agent{}, err
	}
	for _, agent := range r.Agents {
		if agent.ID == resolved {
			return agent, nil
		}
	}
	return Agent{}, fmt.Errorf("agent %q not found in registry", resolved)
}

// CurrentPlatform returns the ACP registry platform key for the current host.
func CurrentPlatform() (string, error) {
	osName := runtime.GOOS
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	default:
		return "", fmt.Errorf("unsupported architecture %q", runtime.GOARCH)
	}
	switch osName {
	case "darwin", "linux", "windows":
		return osName + "-" + arch, nil
	default:
		return "", fmt.Errorf("unsupported OS %q", runtime.GOOS)
	}
}

// BinaryForCurrentPlatform returns the current platform binary target.
func (a Agent) BinaryForCurrentPlatform() (string, BinaryTarget, bool, error) {
	platform, err := CurrentPlatform()
	if err != nil {
		return "", BinaryTarget{}, false, err
	}
	target, ok := a.Distribution.Binary[platform]
	return platform, target, ok, nil
}
