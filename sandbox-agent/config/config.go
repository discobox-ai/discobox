package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/obot-platform/discobox/api/model"
)

const (
	DefaultPath               = "/etc/discobox/sandbox.json"
	ControlPlanePublicKeyName = "controlPlane"
)

type Config struct {
	Identity              Identity          `json:"identity"`
	ControlPlanePublicKey string            `json:"controlPlanePublicKey"`
	ListenAddress         string            `json:"listenAddress"`
	WorkingRoot           string            `json:"workingRoot"`
	ExecDefaults          ExecDefaults      `json:"execDefaults,omitempty"`
	RuntimeDir            string            `json:"runtimeDir"`
	DatabasePath          string            `json:"databasePath"`
	Env                   map[string]string `json:"env,omitempty"`
	Prompt                []string          `json:"prompt,omitempty"`
	HarnessMode           string            `json:"harnessMode,omitempty"`
	ResolvedHarnessConfig *Harness          `json:"resolvedHarnessConfig,omitempty"`
	Harnesses             []Harness         `json:"harnesses"`
	SandboxConfig         map[string]any    `json:"-"`
	Resources             ResourceConfig    `json:"resources"`
}

type Identity struct {
	ProjectID string `json:"projectId"`
	SandboxID string `json:"sandboxId"`
	WorkerID  string `json:"workerId"`
}

type ExecDefaults struct {
	Workdir       string `json:"workdir,omitempty"`
	Username      string `json:"username,omitempty"`
	UID           *int64 `json:"uid,omitempty"`
	GID           *int64 `json:"gid,omitempty"`
	HomeDirectory string `json:"homeDirectory,omitempty"`
}

type Harness struct {
	ID              string        `json:"id"`
	TypeID          string        `json:"-"`
	Name            string        `json:"name"`
	Command         []string      `json:"command"`
	RelaunchCommand []string      `json:"relaunchCommand,omitempty"`
	IsDefault       bool          `json:"isDefault,omitempty"`
	Files           []HarnessFile `json:"files,omitempty"`
}

// HarnessFile is a file to write into the harness's home directory when the harness
// is installed.
type HarnessFile struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	CreateOnly bool   `json:"createOnly,omitempty"`
	Template   bool   `json:"template,omitempty"`
}

type ResourceConfig struct {
	SampleInterval time.Duration `json:"sampleInterval"`
	RetentionCount int           `json:"retentionCount"`
}

func Load(path string) (Config, error) {
	if strings.TrimSpace(path) == "" {
		path = getenv("DISCOBOX_SANDBOX_CONFIG", DefaultPath)
	}
	var cfg Config
	if data, err := os.ReadFile(path); err == nil {
		if err := unmarshalManifest(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse sandbox config %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("read sandbox config %s: %w", path, err)
	}
	applyEnv(&cfg)
	applyDefaults(&cfg)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func unmarshalManifest(data []byte, cfg *Config) error {
	var manifest model.SandboxManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	if strings.TrimSpace(manifest.APIVersion) != model.SandboxManifestAPIVersion {
		return fmt.Errorf("apiVersion = %q, want %q", manifest.APIVersion, model.SandboxManifestAPIVersion)
	}
	var templateData struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(data, &templateData); err != nil {
		return fmt.Errorf("decode public sandbox config template data: %w", err)
	}
	*cfg = configFromManifest(manifest)
	cfg.SandboxConfig = templateData.Config
	return nil
}

func configFromManifest(manifest model.SandboxManifest) Config {
	cfg := Config{
		Identity: Identity{
			SandboxID: manifest.SandboxID,
		},
	}
	if manifest.Provider != nil {
		cfg.Identity.ProjectID = manifest.Provider.ProjectID
		cfg.Identity.WorkerID = manifest.Provider.WorkerID
		cfg.ControlPlanePublicKey = publicKey(manifest.Provider.PublicKeys)
	}
	if env, ok := manifest.Config.Env.Get(); ok {
		cfg.Env = map[string]string(env)
	}
	cfg.Prompt = cloneCommand(manifest.Config.Prompt)
	if mode, ok := manifest.Config.HarnessMode.Get(); ok {
		cfg.HarnessMode = string(mode)
	}
	cfg.ExecDefaults = execDefaultsFromManifestConfig(manifest.Config)
	if manifest.AgentRuntime != nil {
		cfg.ListenAddress = manifest.AgentRuntime.ListenAddress
		cfg.WorkingRoot = manifest.AgentRuntime.WorkingRoot
		cfg.RuntimeDir = manifest.AgentRuntime.RuntimeDir
		cfg.DatabasePath = manifest.AgentRuntime.DatabasePath
		if manifest.AgentRuntime.ResourceCollection != nil {
			if sampleInterval := strings.TrimSpace(manifest.AgentRuntime.ResourceCollection.SampleInterval); sampleInterval != "" {
				if parsed, err := time.ParseDuration(sampleInterval); err == nil {
					cfg.Resources.SampleInterval = parsed
				}
			}
			cfg.Resources.RetentionCount = int(manifest.AgentRuntime.ResourceCollection.RetentionCount)
		}
	}
	if manifest.ResolvedHarnessConfig != nil {
		resolved := agentFromResolvedManifest(*manifest.ResolvedHarnessConfig)
		cfg.ResolvedHarnessConfig = &resolved
	}
	return cfg
}

func execDefaultsFromManifestConfig(config model.SandboxConfig) ExecDefaults {
	var out ExecDefaults
	if source, ok := config.Source.Get(); ok {
		if destination, ok := source.Destination.Get(); ok {
			out.Workdir = strings.TrimSpace(destination.WorkingDirectory.Or(""))
		}
	}
	if user, ok := config.User.Get(); ok {
		out.Username = strings.TrimSpace(user.Name.Or(""))
		out.HomeDirectory = strings.TrimSpace(user.HomeDirectory.Or(""))
		if uid, ok := user.UID.Get(); ok {
			out.UID = int64Ptr(uid)
		}
		if gid, ok := user.Gid.Get(); ok {
			out.GID = int64Ptr(gid)
		} else if user.UID.Set {
			out.GID = int64Ptr(user.UID.Value)
		}
	}
	return out
}

func int64Ptr(value int64) *int64 {
	return &value
}

func publicKey(values map[string]string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[ControlPlanePublicKeyName])
}

func agentFromResolvedManifest(in model.SandboxManifestResolvedHarnessConfig) Harness {
	return Harness{
		ID: in.ID, Name: in.Name, Files: harnessFilesFromManifest(in.Files),
	}
}

func harnessFilesFromManifest(in []model.HarnessConfigFile) []HarnessFile {
	if len(in) == 0 {
		return nil
	}
	out := make([]HarnessFile, 0, len(in))
	for _, file := range in {
		out = append(out, HarnessFile{
			Path:       file.Path,
			Content:    file.Content,
			CreateOnly: file.CreateOnly.Or(false),
			Template:   file.Template.Or(false),
		})
	}
	return out
}

func cloneCommand(command []string) []string {
	if len(command) == 0 {
		return nil
	}
	return append([]string{}, command...)
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Identity.ProjectID) == "" {
		return fmt.Errorf("projectId is required")
	}
	if strings.TrimSpace(c.Identity.SandboxID) == "" {
		return fmt.Errorf("sandboxId is required")
	}
	if strings.TrimSpace(c.ControlPlanePublicKey) == "" {
		return fmt.Errorf("provider.publicKeys.%s is required", ControlPlanePublicKeyName)
	}
	switch c.HarnessMode {
	case "", "run", "config":
	default:
		return fmt.Errorf("unsupported harnessMode %q", c.HarnessMode)
	}
	for _, harness := range c.Harnesses {
		if strings.TrimSpace(harness.ID) == "" {
			return fmt.Errorf("harness id is required")
		}
		if len(harness.Command) == 0 || strings.TrimSpace(harness.Command[0]) == "" {
			return fmt.Errorf("harness %q command is required", harness.ID)
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
		cfg.RuntimeDir = "/run/discobox/harness-terminals"
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
