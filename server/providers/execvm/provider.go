package execvm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/dockerworker"
	"github.com/obot-platform/discobox/server/providers/poolruntime"
)

const (
	ProviderType      = "exec"
	defaultAgentPort  = 3002
	labelProviderType = "discobox.provider_type"
)

// Config is the persisted provider instance configuration.
type Config struct {
	Command          poolruntime.StringList `json:"command,omitempty"`
	ControlPlaneURL  string                 `json:"controlPlaneUrl,omitempty"`
	WorkerImage      string                 `json:"workerImage,omitempty"`
	AgentPort        int                    `json:"agentPort,omitempty"`
	PublicAgentPort  *bool                  `json:"publicAgentPort,omitempty"`
	SSHUser          string                 `json:"sshUser,omitempty"`
	SSHPrivateKey    string                 `json:"sshPrivateKey,omitempty"`
	SSHPrivateKeyEnv string                 `json:"sshPrivateKeyEnv,omitempty"`
}

func Decode(data json.RawMessage) (Config, error) {
	return poolruntime.DecodeConfig[Config](data, ProviderType)
}

func Validate(data json.RawMessage) error {
	cfg, err := Decode(data)
	if err != nil {
		return err
	}
	if len(cfg.Command.Values()) == 0 {
		return fmt.Errorf("exec command is required")
	}
	return poolruntime.RequireControlPlaneURL(ProviderType, cfg.ControlPlaneURL)
}

func FactoryWithPoolManager(poolManager poolruntime.PoolManager) sandbox.ProviderFactory {
	return func(ctx context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return newFromInstance(ctx, instance, poolManager)
	}
}

func newFromInstance(_ context.Context, instance *model.SandboxProviderInstance, poolManager poolruntime.PoolManager) (sandbox.Provider, error) {
	cfg, err := Decode(instance.Config)
	if err != nil {
		return nil, err
	}
	driver, err := NewDriver(DriverConfig{
		Command:       cfg.Command.Values(),
		SSHUser:       cfg.SSHUser,
		SSHPrivateKey: sshPrivateKey(cfg),
	})
	if err != nil {
		return nil, err
	}
	engine, err := dockerworker.New(engineConfig(cfg), driver)
	if err != nil {
		_ = driver.Close()
		return nil, err
	}
	return poolruntime.New(engine, Definition(), poolManager), nil
}

// engineConfig maps the exec provider configuration to the shared engine
// configuration. The harness port publishes on all interfaces by default so the
// harness-endpoint command can return a fixed VM address and port.
func engineConfig(cfg Config) dockerworker.Config {
	publicAgentPort := true
	if cfg.PublicAgentPort != nil {
		publicAgentPort = *cfg.PublicAgentPort
	}
	return dockerworker.Config{
		ControlPlaneURL: cfg.ControlPlaneURL,
		Image:           dockerworker.EffectivePoolImage(cfg.WorkerImage),
		AgentPort:       effectiveAgentPort(cfg.AgentPort),
		PublicAgentPort: publicAgentPort,
		Systemd:         true,
		Labels:          map[string]string{labelProviderType: ProviderType},
	}
}

func effectiveAgentPort(agentPort int) int {
	if agentPort <= 0 {
		return defaultAgentPort
	}
	return agentPort
}

func sshPrivateKey(cfg Config) string {
	if key := strings.TrimSpace(cfg.SSHPrivateKey); key != "" {
		return key
	}
	if envName := strings.TrimSpace(cfg.SSHPrivateKeyEnv); envName != "" {
		return strings.TrimSpace(os.Getenv(envName))
	}
	return ""
}

// Definition describes the exec provider for provider catalogs.
func Definition() sandbox.ProviderDefinition {
	return sandbox.ProviderDefinition{
		Name:        "Exec",
		Icon:        "terminal",
		Description: "Delegates worker VM lifecycle to an external command, such as a shell script.",
		ConfigFields: []sandbox.ProviderConfigField{
			{Key: "command", Label: "Command", Type: "string", Required: true, Description: "Executable (plus fixed arguments) invoked as `<command> <op> <worker-id>`."},
			{Key: "controlPlaneUrl", Label: "Control Plane URL", Type: "string", Required: true, Placeholder: "https://discobot.example.com"},
			{Key: "workerImage", Label: "Worker Image", Type: "string", Placeholder: dockerworker.DefaultPoolImage, Description: "Worker-agent container image launched in the VM's Docker daemon.", Advanced: true},
			{Key: "publicAgentPort", Label: "Publish Harness Port Publicly", Type: "boolean", Description: "Publish the worker-agent port on all VM interfaces at the fixed harness port.", Advanced: true},
			{Key: "sshUser", Label: "SSH User", Type: "string", Placeholder: "root", Advanced: true},
			{Key: "sshPrivateKey", Label: "SSH Private Key", Type: "password", Description: "PEM private key used when docker-endpoint returns ssh:// URLs.", Advanced: true},
			{Key: "sshPrivateKeyEnv", Label: "SSH Private Key Environment Variable", Type: "string", Advanced: true},
			{Key: "agentPort", Label: "Harness Port", Type: "number", Placeholder: strconv.Itoa(defaultAgentPort), Advanced: true},
		},
	}
}
