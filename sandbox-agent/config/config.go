package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/obot-platform/discobox/harness"
	"github.com/obot-platform/discobox/sandboxconfig"
)

const (
	DefaultPath = "/etc/discobox/sandbox.json"
)

// Config is sandbox-agent's decode target for /etc/discobox/sandbox.json.
// Pool-agent has already computed the sandbox's one effective configuration
// via sandboxconfig.Effective before writing the file (ADR 0012 §8);
// sandbox-agent reads it once, statically, and never merges anything itself.
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
	// Harness is the sandbox's one resolved harness. A zero-value Harness
	// (empty ID) means the sandbox has no harness configured.
	Harness       Harness          `json:"harness"`
	Volumes       []harness.Volume `json:"volumes,omitempty"`
	SandboxConfig map[string]any   `json:"-"`
	Resources     ResourceConfig   `json:"resources"`
}

type Identity struct {
	ProjectID string `json:"projectId"`
	SandboxID string `json:"sandboxId"`
	PoolID    string `json:"poolId"`
}

type ExecDefaults struct {
	Workdir       string `json:"workdir,omitempty"`
	Username      string `json:"username,omitempty"`
	UID           *int64 `json:"uid,omitempty"`
	GID           *int64 `json:"gid,omitempty"`
	HomeDirectory string `json:"homeDirectory,omitempty"`
	// AdditionalGroups are the manifest's supplementary groups for this user.
	// The manifest is the source of truth for membership; the OS group file is
	// consulted only to resolve a name to a GID.
	AdditionalGroups []string `json:"additionalGroups,omitempty"`
}

// Harness is the sandbox's one effective, fully-resolved harness.
type Harness struct {
	ID              string        `json:"id"`
	TypeID          string        `json:"-"`
	Name            string        `json:"name"`
	Command         []string      `json:"command"`
	RelaunchCommand []string      `json:"relaunchCommand,omitempty"`
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
		path = DefaultPath
	}
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return Config{}, fmt.Errorf("read sandbox config %s: %w", path, err)
		}
	} else {
		if err := unmarshalConfig(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse sandbox config %s: %w", path, err)
		}
	}
	applyDefaults(&cfg)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func unmarshalConfig(data []byte, cfg *Config) error {
	var effective sandboxconfig.Config
	if err := json.Unmarshal(data, &effective); err != nil {
		return err
	}
	if strings.TrimSpace(effective.APIVersion) != sandboxconfig.APIVersion {
		return fmt.Errorf("apiVersion = %q, want %q", effective.APIVersion, sandboxconfig.APIVersion)
	}
	// The whole document also becomes the harness file template data (ADR
	// 0012's Effective(Document) fields, decoded generically): harness image
	// files marked "template": true render against it.
	var templateData map[string]any
	if err := json.Unmarshal(data, &templateData); err != nil {
		return fmt.Errorf("decode template data: %w", err)
	}
	*cfg = configFromEffective(effective)
	cfg.SandboxConfig = templateData
	return nil
}

func configFromEffective(effective sandboxconfig.Config) Config {
	harnessCommand := effective.Harness.RunCommand
	if effective.HarnessMode == "config" {
		harnessCommand = effective.Harness.ConfigCommand
	}
	cfg := Config{
		Identity: Identity{
			SandboxID: effective.SandboxID,
			ProjectID: effective.Provider.ProjectID,
			PoolID:    effective.Provider.PoolID,
		},
		ControlPlanePublicKey: publicKey(effective.Provider.PublicKeys),
		ListenAddress:         effective.AgentRuntime.ListenAddress,
		WorkingRoot:           effective.AgentRuntime.WorkingRoot,
		RuntimeDir:            effective.AgentRuntime.RuntimeDir,
		DatabasePath:          effective.AgentRuntime.DatabasePath,
		Env:                   effective.Env,
		Prompt:                cloneCommand(effective.Prompt),
		HarnessMode:           effective.HarnessMode,
		Volumes:               effective.Volumes,
	}
	if sampleInterval := strings.TrimSpace(effective.AgentRuntime.ResourceSampleInterval); sampleInterval != "" {
		if parsed, err := time.ParseDuration(sampleInterval); err == nil {
			cfg.Resources.SampleInterval = parsed
		}
	}
	cfg.Resources.RetentionCount = effective.AgentRuntime.ResourceRetentionCount
	cfg.ExecDefaults = execDefaultsFromEffective(effective)
	if strings.TrimSpace(effective.Harness.ID) != "" {
		cfg.Harness = Harness{
			ID:              effective.Harness.ID,
			TypeID:          effective.Harness.ID,
			Name:            effective.Harness.Name,
			Command:         cloneCommand(harnessCommand),
			RelaunchCommand: cloneCommand(effective.Harness.RelaunchCommand),
			Files:           harnessFilesFromEffective(effective.Files),
		}
	}
	return cfg
}

// execDefaultsFromEffective derives the default exec workdir and user from
// the effective config: the workdir is the primary source's target (falling
// back to the runtime working root), matching how pool-agent resolves the
// sandbox user for the same mount.
func execDefaultsFromEffective(effective sandboxconfig.Config) ExecDefaults {
	var out ExecDefaults
	for _, source := range effective.Sources {
		if source.Slug == "primary" {
			out.Workdir = source.Target
			break
		}
	}
	out.Username = strings.TrimSpace(effective.User.Name)
	out.HomeDirectory = strings.TrimSpace(effective.User.HomeDirectory)
	out.UID = effective.User.UID
	out.GID = effective.User.GID
	out.AdditionalGroups = append([]string(nil), effective.AdditionalGroups...)
	return out
}

func publicKey(values map[string]string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[ControlPlanePublicKeyName])
}

const ControlPlanePublicKeyName = "controlPlane"

func harnessFilesFromEffective(in []sandboxconfig.File) []HarnessFile {
	if len(in) == 0 {
		return nil
	}
	out := make([]HarnessFile, 0, len(in))
	for _, file := range in {
		out = append(out, HarnessFile{
			Path:       file.Path,
			Content:    file.Content,
			CreateOnly: file.CreateOnly,
			Template:   file.Template,
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
	if strings.TrimSpace(c.Harness.ID) != "" && (len(c.Harness.Command) == 0 || strings.TrimSpace(c.Harness.Command[0]) == "") {
		return fmt.Errorf("harness %q command is required", c.Harness.ID)
	}
	return nil
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
