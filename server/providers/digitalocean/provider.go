// Package digitalocean registers the "digitalocean" provider type: one
// Droplet per worker, running Docker. It is the reference VM driver for the
// shared dockerworker engine: droplet CRUD keyed by worker tag, the in-VM
// Docker daemon reached over SSH, and the worker-agent API reached at the
// droplet's public address.
package digitalocean

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
	ProviderType      = "digitalocean"
	defaultAPIBaseURL = "https://api.digitalocean.com"
	defaultRegion     = "nyc3"
	defaultSize       = "s-2vcpu-4gb"
	defaultImage      = "ubuntu-24-04-x64"
	defaultAgentPort  = 3002
	defaultSSHUser    = "root"
	labelProviderType = "discobox.provider_type"
)

// Config is the persisted provider instance configuration.
type Config struct {
	Token            string                 `json:"token,omitempty"`
	TokenEnv         string                 `json:"tokenEnv,omitempty"`
	ControlPlaneURL  string                 `json:"controlPlaneUrl,omitempty"`
	APIBaseURL       string                 `json:"apiBaseUrl,omitempty"`
	Region           string                 `json:"region,omitempty"`
	Size             string                 `json:"size,omitempty"`
	Image            string                 `json:"image,omitempty"`
	WorkerImage      string                 `json:"workerImage,omitempty"`
	SSHKeys          poolruntime.StringList `json:"sshKeys,omitempty"`
	SSHUser          string                 `json:"sshUser,omitempty"`
	SSHPrivateKey    string                 `json:"sshPrivateKey,omitempty"`
	SSHPrivateKeyEnv string                 `json:"sshPrivateKeyEnv,omitempty"`
	VPCUUID          string                 `json:"vpcUuid,omitempty"`
	Tags             poolruntime.StringList `json:"tags,omitempty"`
	Backups          bool                   `json:"backups,omitempty"`
	IPv6             bool                   `json:"ipv6,omitempty"`
	Monitoring       bool                   `json:"monitoring,omitempty"`
	AgentPort        int                    `json:"agentPort,omitempty"`
}

func Decode(data json.RawMessage) (Config, error) {
	return poolruntime.DecodeConfig[Config](data, ProviderType)
}

func Validate(data json.RawMessage) error {
	cfg, err := Decode(data)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Token) == "" && strings.TrimSpace(cfg.TokenEnv) == "" {
		return fmt.Errorf("digitalocean token or tokenEnv is required")
	}
	if accessSecret(cfg.SSHPrivateKey, cfg.SSHPrivateKeyEnv, "") == "" {
		return fmt.Errorf("digitalocean sshPrivateKey or sshPrivateKeyEnv is required to reach worker Docker daemons")
	}
	return poolruntime.RequireControlPlaneURL(ProviderType, cfg.ControlPlaneURL)
}

func FactoryWithPoolManager(poolManager poolruntime.PoolManager, imageSync *dockerworker.DevelopmentImageSynchronizer) sandbox.ProviderFactory {
	return func(ctx context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return newFromInstance(ctx, instance, poolManager, imageSync)
	}
}

func newFromInstance(_ context.Context, instance *model.SandboxProviderInstance, poolManager poolruntime.PoolManager, imageSync *dockerworker.DevelopmentImageSynchronizer) (sandbox.Provider, error) {
	cfg, err := Decode(instance.Config)
	if err != nil {
		return nil, err
	}
	driver, err := NewDriver(driverConfigFrom(cfg))
	if err != nil {
		return nil, err
	}
	engineCfg := engineConfig(cfg)
	engineCfg.DevelopmentImageSync = imageSync
	engine, err := dockerworker.New(engineCfg, driver)
	if err != nil {
		_ = driver.Close()
		return nil, err
	}
	return poolruntime.New(engine, Definition(), poolManager), nil
}

func driverConfigFrom(cfg Config) DriverConfig {
	return DriverConfig{
		Token:         accessSecret(cfg.Token, cfg.TokenEnv, "DIGITALOCEAN_ACCESS_TOKEN"),
		APIBaseURL:    cfg.APIBaseURL,
		Region:        cfg.Region,
		Size:          cfg.Size,
		Image:         cfg.Image,
		SSHKeys:       cfg.SSHKeys.Values(),
		SSHUser:       cfg.SSHUser,
		SSHPrivateKey: accessSecret(cfg.SSHPrivateKey, cfg.SSHPrivateKeyEnv, ""),
		VPCUUID:       cfg.VPCUUID,
		Tags:          cfg.Tags.Values(),
		Backups:       cfg.Backups,
		IPv6:          cfg.IPv6,
		Monitoring:    cfg.Monitoring,
		AgentPort:     effectiveAgentPort(cfg.AgentPort),
	}
}

// engineConfig maps the DigitalOcean provider configuration to the shared
// engine configuration. The worker-agent container publishes its port on all
// interfaces so the control plane reaches it at the droplet's public address.
func engineConfig(cfg Config) dockerworker.Config {
	return dockerworker.Config{
		ControlPlaneURL: cfg.ControlPlaneURL,
		Image:           dockerworker.EffectivePoolImage(cfg.WorkerImage),
		AgentPort:       effectiveAgentPort(cfg.AgentPort),
		PublicAgentPort: true,
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

// accessSecret resolves a secret from direct configuration, a configured
// environment variable, or a default environment variable.
func accessSecret(value, valueEnv, defaultEnv string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	envName := strings.TrimSpace(valueEnv)
	if envName == "" {
		envName = defaultEnv
	}
	if envName == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(envName))
}

// Definition describes the DigitalOcean provider for provider catalogs.
func Definition() sandbox.ProviderDefinition {
	return sandbox.ProviderDefinition{
		Name:        "DigitalOcean",
		Icon:        "digitalocean",
		Description: "Runs one Docker-enabled Droplet per sandbox worker.",
		ConfigFields: []sandbox.ProviderConfigField{
			{Key: "token", Label: "API Token", Type: "password", CredentialProvider: "digitalocean", CredentialAuthType: "token"},
			{Key: "tokenEnv", Label: "API Token Environment Variable", Type: "string", Placeholder: "DIGITALOCEAN_ACCESS_TOKEN", Description: "Environment variable containing the API token; use instead of token for local CLI workflows."},
			{Key: "controlPlaneUrl", Label: "Control Plane URL", Type: "string", Required: true, Placeholder: "https://discobot.example.com"},
			{Key: "sshKeys", Label: "SSH Keys", Type: "string", Required: true, Description: "SSH key IDs or fingerprints registered with DigitalOcean; must match the SSH private key."},
			{Key: "sshPrivateKey", Label: "SSH Private Key", Type: "password", Description: "PEM private key used to reach the Droplet's Docker daemon over SSH."},
			{Key: "sshPrivateKeyEnv", Label: "SSH Private Key Environment Variable", Type: "string", Description: "Environment variable containing the SSH private key; use instead of sshPrivateKey."},
			{Key: "sshUser", Label: "SSH User", Type: "string", Placeholder: defaultSSHUser, Advanced: true},
			{Key: "region", Label: "Region", Type: "string", Placeholder: defaultRegion},
			{Key: "size", Label: "Droplet Size", Type: "string", Placeholder: defaultSize},
			{Key: "image", Label: "Droplet Image", Type: "string", Placeholder: defaultImage},
			{Key: "workerImage", Label: "Worker Image", Type: "string", Placeholder: dockerworker.DefaultPoolImage, Description: "Worker-agent container image launched in the Droplet's Docker daemon.", Advanced: true},
			{Key: "vpcUuid", Label: "VPC UUID", Type: "string", Advanced: true},
			{Key: "tags", Label: "Tags", Type: "string", Advanced: true},
			{Key: "backups", Label: "Backups", Type: "boolean", Advanced: true},
			{Key: "ipv6", Label: "IPv6", Type: "boolean", Advanced: true},
			{Key: "monitoring", Label: "Monitoring", Type: "boolean", Advanced: true},
			{Key: "agentPort", Label: "Harness Port", Type: "number", Placeholder: strconv.Itoa(defaultAgentPort), Advanced: true},
		},
	}
}
