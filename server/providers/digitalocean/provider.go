// Package digitalocean registers the "digitalocean" provider type: one
// Droplet per pool, running Docker. It is the reference VM driver for the
// shared dockerworker engine: droplet CRUD keyed by a per-pool tag, the in-VM
// Docker daemon reached over SSH, and the pool-agent API reached at the
// droplet's public address.
package digitalocean

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport/carrierhub"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
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
	poolruntime.PoolPolicy

	Token            string                 `json:"token,omitempty"`
	ControlPlaneURL  string                 `json:"controlPlaneUrl,omitempty"`
	APIBaseURL       string                 `json:"apiBaseUrl,omitempty"`
	Region           string                 `json:"region,omitempty"`
	Size             string                 `json:"size,omitempty"`
	Image            string                 `json:"image,omitempty"`
	SandboxBaseImage string                 `json:"sandboxBaseImage,omitempty"`
	SSHKeys          poolruntime.StringList `json:"sshKeys,omitempty"`
	SSHUser          string                 `json:"sshUser,omitempty"`
	SSHPrivateKey    string                 `json:"sshPrivateKey,omitempty"`
	VPCUUID          string                 `json:"vpcUuid,omitempty"`
	Tags             poolruntime.StringList `json:"tags,omitempty"`
	Backups          bool                   `json:"backups,omitempty"`
	IPv6             bool                   `json:"ipv6,omitempty"`
	Monitoring       bool                   `json:"monitoring,omitempty"`
	AgentPort        int                    `json:"agentPort,omitempty"`
}

func Decode(data []byte) (Config, error) {
	return poolruntime.DecodeConfig[Config](data, ProviderType)
}

func Validate(data json.RawMessage) error {
	if err := poolruntime.RefuseEnvironmentReferences(data, "tokenEnv", "sshPrivateKeyEnv"); err != nil {
		return err
	}
	cfg, err := Decode(data)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return fmt.Errorf("digitalocean token is required")
	}
	if strings.TrimSpace(cfg.SSHPrivateKey) == "" {
		return fmt.Errorf("digitalocean sshPrivateKey is required to reach worker Docker daemons")
	}
	return nil
}

// FactoryWithPoolManager makes the provider's instances. hub is the server's
// carrier hub, which a pool with no controlPlaneUrl is reached through (ADR
// 26-10-30-875).
func FactoryWithPoolManager(poolManager poolruntime.PoolManager, imageSync *dockerworker.DevelopmentImageSynchronizer, serverDefaults dockerworker.ServerDefaults, hub *carrierhub.Hub) sandbox.ProviderFactory {
	return func(ctx context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return newFromInstance(ctx, instance, poolManager, imageSync, serverDefaults, hub)
	}
}

func newFromInstance(_ context.Context, instance *model.SandboxProviderInstance, poolManager poolruntime.PoolManager, imageSync *dockerworker.DevelopmentImageSynchronizer, serverDefaults dockerworker.ServerDefaults, hub *carrierhub.Hub) (sandbox.Provider, error) {
	cfg, err := Decode(instance.Config)
	if err != nil {
		return nil, err
	}
	streams, err := poolruntime.ControlPlaneStreams(ProviderType, cfg.ControlPlaneURL, hub)
	if err != nil {
		return nil, err
	}
	driver, err := NewDriver(driverConfigFrom(cfg))
	if err != nil {
		return nil, err
	}
	engineCfg := engineConfig(cfg, serverDefaults)
	engineCfg.DevelopmentImageSync = imageSync
	engineCfg.ProgressReporter = sandbox.PoolProgressReporterFor(poolManager)
	engine, err := dockerworker.New(engineCfg, driver)
	if err != nil {
		_ = driver.Close()
		return nil, err
	}
	return poolruntime.New(engine, Definition(), poolManager, streams), nil
}

func driverConfigFrom(cfg Config) DriverConfig {
	return DriverConfig{
		Token:         strings.TrimSpace(cfg.Token),
		APIBaseURL:    cfg.APIBaseURL,
		Region:        cfg.Region,
		Size:          cfg.Size,
		Image:         cfg.Image,
		SSHKeys:       cfg.SSHKeys.Values(),
		SSHUser:       cfg.SSHUser,
		SSHPrivateKey: strings.TrimSpace(cfg.SSHPrivateKey),
		VPCUUID:       cfg.VPCUUID,
		Tags:          cfg.Tags.Values(),
		Backups:       cfg.Backups,
		IPv6:          cfg.IPv6,
		Monitoring:    cfg.Monitoring,
		AgentPort:     effectiveAgentPort(cfg.AgentPort),
	}
}

// engineConfig maps the DigitalOcean provider configuration to the shared
// engine configuration. The pool-agent container publishes its port on all
// interfaces so the control plane reaches it at the droplet's public address.
func engineConfig(cfg Config, serverDefaults dockerworker.ServerDefaults) dockerworker.Config {
	return dockerworker.Config{
		ControlPlaneURL:     cfg.ControlPlaneURL,
		SandboxBaseImage:    dockerworker.EffectiveSandboxBaseImage(cfg.SandboxBaseImage, serverDefaults),
		ImageRetention:      serverDefaults.ImageRetention,
		ImageCache:          serverDefaults.ImageCache,
		AgentPort:           effectiveAgentPort(cfg.AgentPort),
		PublicAgentPort:     true,
		Labels:              map[string]string{labelProviderType: ProviderType},
		ProxyAuditRetention: cfg.ProxyAuditRetention.Value(),
		AuditSpoolBudget:    cfg.AuditSpoolBudget,
		SandboxIdleTimeout:  cfg.SandboxIdleTimeout.Value(),
	}
}

func effectiveAgentPort(agentPort int) int {
	if agentPort <= 0 {
		return defaultAgentPort
	}
	return agentPort
}

// Definition describes the DigitalOcean provider for provider catalogs.
func Definition() sandbox.ProviderDefinition {
	return sandbox.ProviderDefinition{
		Name:        "DigitalOcean",
		Icon:        "digitalocean",
		Description: "Runs one Docker-enabled Droplet per sandbox worker.",
		ConfigFields: append([]sandbox.ProviderConfigField{
			{Key: "token", Label: "API Token", Type: sandbox.ProviderConfigFieldSecret, Required: true, Description: "DigitalOcean API token the server manages Droplets with."},
			{Key: "controlPlaneUrl", Label: "Control Plane URL", Type: "string", Placeholder: "https://discobox.example.com", Description: "Where its pools reach this server. Leave it empty and this server opens a channel to each pool instead, so it needs no public URL."},
			{Key: "sshKeys", Label: "SSH Keys", Type: "string", Required: true, Description: "SSH key IDs or fingerprints registered with DigitalOcean; must match the SSH private key."},
			{Key: "sshPrivateKey", Label: "SSH Private Key", Type: sandbox.ProviderConfigFieldSecret, Required: true, Description: "PEM private key used to reach the Droplet's Docker daemon over SSH."},
			{Key: "sshUser", Label: "SSH User", Type: "string", Placeholder: defaultSSHUser, Advanced: true},
			{Key: "region", Label: "Region", Type: "string", Placeholder: defaultRegion},
			{Key: "size", Label: "Droplet Size", Type: "string", Placeholder: defaultSize},
			{Key: "image", Label: "Droplet Image", Type: "string", Placeholder: defaultImage},
			{Key: "sandboxBaseImage", Label: "Sandbox Base Image", Type: "string", Placeholder: dockerworker.DefaultSandboxBaseImage, Description: "Sandbox base image every sandbox delivered its platform runs, and the pool's own container runs, provisioned as a pool (ADR 26-10-16-423 §§7, 10, ADR 26-10-24-953 §2).", Advanced: true},
			{Key: "vpcUuid", Label: "VPC UUID", Type: "string", Advanced: true},
			{Key: "tags", Label: "Tags", Type: "string", Advanced: true},
			{Key: "backups", Label: "Backups", Type: "boolean", Advanced: true},
			{Key: "ipv6", Label: "IPv6", Type: "boolean", Advanced: true},
			{Key: "monitoring", Label: "Monitoring", Type: "boolean", Advanced: true},
			{Key: "agentPort", Label: "Harness Port", Type: "number", Placeholder: strconv.Itoa(defaultAgentPort), Advanced: true},
		}, poolruntime.PoolPolicyConfigFields()...),
	}
}
