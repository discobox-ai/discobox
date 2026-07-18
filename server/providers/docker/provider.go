// Package docker registers the "docker" provider type: workers run as
// containers on the local Docker daemon. It provides the local VM driver for
// the shared dockerworker engine — VM CRUD is a no-op because the host is the
// "VM" — plus a runtime drift watcher over the shared daemon.
package docker

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"github.com/obot-platform/discobox/controlplane"
	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/dockerworker"
	"github.com/obot-platform/discobox/server/providers/poolruntime"
)

const (
	ProviderType      = "docker"
	defaultAgentPort  = 3002
	dockerHostGateway = "host.docker.internal"
	dockerSocketPath  = "/var/run/docker.sock"
	labelProviderType = "discobox.provider_type"
)

// DefaultImage returns the default Docker worker image.
func DefaultImage() string { return dockerworker.DefaultPoolImage }

// DefaultAgentPort returns the default worker-agent port exposed by Docker workers.
func DefaultAgentPort() int { return defaultAgentPort }

// Config is the persisted provider instance configuration.
type Config struct {
	ControlPlaneURL string                   `json:"controlPlaneUrl,omitempty"`
	Host            string                   `json:"host,omitempty"`
	Image           string                   `json:"image,omitempty"`
	Network         string                   `json:"network,omitempty"`
	AgentPort       int                      `json:"agentPort,omitempty"`
	Systemd         *bool                    `json:"systemd,omitempty"`
	Privileged      *bool                    `json:"privileged,omitempty"`
	CgroupNSMode    string                   `json:"cgroupNsMode,omitempty"`
	Command         poolruntime.StringList   `json:"command,omitempty"`
	DockerSocket    string                   `json:"bindDockerSocket,omitempty"`
	HostMounts      []dockerworker.HostMount `json:"hostMounts,omitempty"`
}

// HostMount aliases the engine host mount type for provider configuration.
type HostMount = dockerworker.HostMount

func Decode(data json.RawMessage) (Config, error) {
	return poolruntime.DecodeConfig[Config](data, ProviderType)
}

func Validate(data json.RawMessage) error {
	_, err := Decode(data)
	return err
}

func FactoryWithPoolManager(poolManager poolruntime.PoolManager) sandbox.ProviderFactory {
	return func(ctx context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return newFromInstance(ctx, instance, poolManager)
	}
}

func newFromInstance(ctx context.Context, instance *model.SandboxProviderInstance, poolManager poolruntime.PoolManager) (sandbox.Provider, error) {
	cfg, err := Decode(instance.Config)
	if err != nil {
		return nil, err
	}
	driver, err := NewLocalDriver(ctx, cfg.Host, effectiveAgentPort(cfg.AgentPort))
	if err != nil {
		return nil, err
	}
	engine, err := dockerworker.New(engineConfig(cfg), driver)
	if err != nil {
		_ = driver.Close()
		return nil, err
	}
	if err := startPoolWatcher(driver, engine, poolManager, instance); err != nil {
		_ = engine.Close()
		return nil, err
	}
	definition := Definition()
	definition.LocalSourceBind = localSourceBindSupported(driver.DaemonHost())
	return poolruntime.New(engine, definition, poolManager), nil
}

// localSourceBindSupported reports whether containers on daemonHost share a
// filesystem with this process, so a local source directory can be bind-mounted
// into a sandbox.
//
// Only socket transports qualify. A socket means the daemon is on this machine,
// including Docker Desktop, which shares host paths into its VM transparently.
// ssh:// and remote tcp:// daemons cannot see these files at all, and a
// tcp://localhost daemon may be forwarded anywhere, so none of them qualify:
// binding a path the daemon cannot resolve fails at run time, while declining
// costs only a source push.
func localSourceBindSupported(daemonHost string) bool {
	scheme, _, ok := strings.Cut(strings.TrimSpace(daemonHost), "://")
	if !ok {
		return false
	}
	switch scheme {
	case "unix", "npipe":
		return true
	default:
		return false
	}
}

// engineConfig maps the docker provider configuration to the shared engine
// configuration, applying local-Docker defaults such as the host-gateway
// control plane URL.
func engineConfig(cfg Config) dockerworker.Config {
	controlPlaneURL := strings.TrimSpace(cfg.ControlPlaneURL)
	var extraHosts []string
	if controlPlaneURL == "" {
		controlPlaneURL = defaultDockerControlPlaneURL()
		if controlPlaneURLUsesHostGateway(controlPlaneURL) {
			extraHosts = []string{dockerHostGateway + ":host-gateway"}
		}
	}
	return dockerworker.Config{
		ControlPlaneURL: controlPlaneURL,
		Image:           dockerworker.EffectivePoolImage(cfg.Image),
		Network:         cfg.Network,
		AgentPort:       effectiveAgentPort(cfg.AgentPort),
		Systemd:         cfg.systemdValue(),
		Privileged:      cfg.Privileged,
		CgroupNSMode:    cfg.CgroupNSMode,
		Command:         cfg.Command.Values(),
		DockerSocket:    cfg.DockerSocket,
		HostMounts:      cfg.HostMounts,
		ExtraHosts:      extraHosts,
		Labels:          map[string]string{labelProviderType: ProviderType},
	}
}

func effectiveAgentPort(agentPort int) int {
	if agentPort <= 0 {
		return defaultAgentPort
	}
	return agentPort
}

func (c Config) systemdValue() bool {
	if c.Systemd == nil {
		return true
	}
	return *c.Systemd
}

func defaultDockerControlPlaneURL() string {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = strconv.Itoa(controlplane.DefaultPort)
	}
	return "http://" + dockerHostGateway + ":" + port
}

func controlPlaneURLUsesHostGateway(value string) bool {
	return strings.Contains(value, "://"+dockerHostGateway) || strings.HasPrefix(value, dockerHostGateway+":")
}

func EffectivePoolImage(image string) string {
	return dockerworker.EffectivePoolImage(image)
}

func PoolImageSource(image string) string {
	return dockerworker.PoolImageSource(image)
}

// Definition describes the Docker provider for provider catalogs.
func Definition() sandbox.ProviderDefinition {
	return sandbox.ProviderDefinition{
		Name:        "Docker",
		Icon:        "docker",
		Description: "Runs VM-style workers as Docker containers, optionally with systemd as PID 1.",
		ConfigFields: []sandbox.ProviderConfigField{
			{Key: "controlPlaneUrl", Label: "Control Plane URL", Type: "string", Placeholder: controlplane.DefaultURL(dockerHostGateway, controlplane.DefaultPort), Advanced: true},
			{Key: "host", Label: "Docker Host", Type: "string", Advanced: true},
			{Key: "image", Label: "Image", Type: "string", Placeholder: dockerworker.DefaultPoolImage},
			{Key: "network", Label: "Docker Network", Type: "string", Advanced: true},
			{Key: "systemd", Label: "Run systemd", Type: "boolean", Advanced: true},
			{Key: "privileged", Label: "Privileged", Type: "boolean", Advanced: true},
			{Key: "cgroupNsMode", Label: "Cgroup Namespace", Type: "string", Advanced: true},
			{Key: "command", Label: "Command", Type: "string", Advanced: true},
			{Key: "bindDockerSocket", Label: "Bind Docker Socket", Type: "string", Placeholder: dockerSocketPath, Advanced: true},
			{Key: "agentPort", Label: "Harness Port", Type: "number", Placeholder: strconv.Itoa(defaultAgentPort), Advanced: true},
		},
	}
}
