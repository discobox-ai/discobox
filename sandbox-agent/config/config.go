package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const DefaultPath = "/etc/discobox/sandbox-agent.json"

type Config struct {
	Identity              Identity       `json:"identity"`
	ControlPlanePublicKey string         `json:"controlPlanePublicKey"`
	ListenAddress         string         `json:"listenAddress"`
	WorkingRoot           string         `json:"workingRoot"`
	RuntimeDir            string         `json:"runtimeDir"`
	DatabasePath          string         `json:"databasePath"`
	Agents                []Agent        `json:"agents"`
	Resources             ResourceConfig `json:"resources"`
}

type Identity struct {
	ProjectID string `json:"projectId"`
	SandboxID string `json:"sandboxId"`
	WorkerID  string `json:"workerId"`
}

type Agent struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Command []string `json:"command"`
}

type ResourceConfig struct {
	SampleInterval time.Duration `json:"sampleInterval"`
	RetentionCount int           `json:"retentionCount"`
}

func Load(path string) (Config, error) {
	if strings.TrimSpace(path) == "" {
		path = getenv("DISCOBOX_SANDBOX_AGENT_CONFIG", DefaultPath)
	}
	var cfg Config
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse sandbox-agent config %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("read sandbox-agent config %s: %w", path, err)
	}
	applyEnv(&cfg)
	applyDefaults(&cfg)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Identity.ProjectID) == "" {
		return fmt.Errorf("projectId is required")
	}
	if strings.TrimSpace(c.Identity.SandboxID) == "" {
		return fmt.Errorf("sandboxId is required")
	}
	if strings.TrimSpace(c.Identity.WorkerID) == "" {
		return fmt.Errorf("workerId is required")
	}
	if strings.TrimSpace(c.ControlPlanePublicKey) == "" {
		return fmt.Errorf("controlPlanePublicKey is required")
	}
	for _, agent := range c.Agents {
		if strings.TrimSpace(agent.ID) == "" {
			return fmt.Errorf("agent id is required")
		}
		if len(agent.Command) == 0 || strings.TrimSpace(agent.Command[0]) == "" {
			return fmt.Errorf("agent %q command is required", agent.ID)
		}
	}
	return nil
}

func applyEnv(cfg *Config) {
	cfg.Identity.ProjectID = getenv("DISCOBOX_PROJECT_ID", cfg.Identity.ProjectID)
	cfg.Identity.SandboxID = getenv("DISCOBOX_SANDBOX_ID", cfg.Identity.SandboxID)
	cfg.Identity.WorkerID = getenv("DISCOBOX_WORKER_ID", cfg.Identity.WorkerID)
	cfg.ControlPlanePublicKey = getenv("DISCOBOX_CONTROL_PLANE_PUBLIC_KEY", cfg.ControlPlanePublicKey)
	cfg.ListenAddress = getenv("DISCOBOX_SANDBOX_AGENT_ADDR", cfg.ListenAddress)
	cfg.WorkingRoot = getenv("DISCOBOX_WORKING_ROOT", cfg.WorkingRoot)
	cfg.RuntimeDir = getenv("DISCOBOX_RUNTIME_DIR", cfg.RuntimeDir)
	cfg.DatabasePath = getenv("DISCOBOX_DATABASE_PATH", cfg.DatabasePath)
	if value := strings.TrimSpace(os.Getenv("DISCOBOX_RESOURCE_RETENTION_COUNT")); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			cfg.Resources.RetentionCount = parsed
		}
	}
}

func applyDefaults(cfg *Config) {
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = ":3003"
	}
	if cfg.WorkingRoot == "" {
		cfg.WorkingRoot = "/workspace"
	}
	if cfg.RuntimeDir == "" {
		cfg.RuntimeDir = "/run/discobox/agent-terminals"
	}
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = "/var/lib/discobox/sandbox-agent.db"
	}
	if cfg.Resources.SampleInterval <= 0 {
		cfg.Resources.SampleInterval = time.Second
	}
	if cfg.Resources.RetentionCount <= 0 {
		cfg.Resources.RetentionCount = 300
	}
}

func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
