// Package docker registers the "docker" provider type: workers run as
// containers on the local Docker daemon. It provides the local VM driver for
// the shared dockerworker engine — VM CRUD is a no-op because the host is the
// "VM" — plus a runtime drift watcher over the shared daemon.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/obot-platform/discobox/controlplane"
	"github.com/obot-platform/discobox/localipc"
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

func FactoryWithPoolManager(poolManager poolruntime.PoolManager, imageSync *dockerworker.DevelopmentImageSynchronizer, listenEndpoints []string) sandbox.ProviderFactory {
	return func(ctx context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return newFromInstance(ctx, instance, poolManager, imageSync, listenEndpoints)
	}
}

func newFromInstance(ctx context.Context, instance *model.SandboxProviderInstance, poolManager poolruntime.PoolManager, imageSync *dockerworker.DevelopmentImageSynchronizer, listenEndpoints []string) (sandbox.Provider, error) {
	cfg, err := Decode(instance.Config)
	if err != nil {
		return nil, err
	}
	driver, err := NewLocalDriver(ctx, cfg.Host, effectiveAgentPort(cfg.AgentPort))
	if err != nil {
		return nil, err
	}
	engineCfg, err := engineConfig(cfg, listenEndpoints, driver.DaemonHost())
	if err != nil {
		_ = driver.Close()
		return nil, err
	}
	engineCfg.DevelopmentImageSync = imageSync
	engine, err := dockerworker.New(engineCfg, driver)
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
// configuration, deriving how the pool agent reaches the control plane when the
// instance does not name it.
func engineConfig(cfg Config, listenEndpoints []string, daemonHost string) (dockerworker.Config, error) {
	engineCfg := dockerworker.Config{
		ControlPlaneURL: strings.TrimSpace(cfg.ControlPlaneURL),
		Image:           dockerworker.EffectivePoolImage(cfg.Image),
		Network:         cfg.Network,
		AgentPort:       effectiveAgentPort(cfg.AgentPort),
		Systemd:         cfg.systemdValue(),
		Privileged:      cfg.Privileged,
		CgroupNSMode:    cfg.CgroupNSMode,
		Command:         cfg.Command.Values(),
		DockerSocket:    cfg.DockerSocket,
		HostMounts:      cfg.HostMounts,
		Labels:          map[string]string{labelProviderType: ProviderType},
	}
	if engineCfg.ControlPlaneURL == "" {
		reach, err := resolveControlPlaneReach(listenEndpoints, daemonHost)
		if err != nil {
			return dockerworker.Config{}, err
		}
		engineCfg.ControlPlaneURL = reach.url
		engineCfg.RelaySocketDir = reach.socketDir
	}
	if controlPlaneURLUsesHostGateway(engineCfg.ControlPlaneURL) {
		engineCfg.ExtraHosts = []string{dockerHostGateway + ":host-gateway"}
	}
	return engineCfg, nil
}

// controlPlaneReach is how a pool container addresses the control plane: the
// URL the agent dials, plus the directory the engine must bind for a socket
// transport to exist inside the container at all.
type controlPlaneReach struct {
	url       string
	socketDir string
}

// resolveControlPlaneReach picks the transport from what this server is actually
// listening on, rather than assuming a TCP port exists.
//
// The local socket comes first when the daemon is on this machine: the socket
// is always listening, it is reachable by a plain bind mount, and preferring it
// is what lets the server default to opening no TCP port at all. A remote
// daemon cannot see the socket, so only an HTTP listener can serve it — and
// when none does, that is a configuration error worth naming here rather than a
// pool that starts and never registers.
func resolveControlPlaneReach(listenEndpoints []string, daemonHost string) (controlPlaneReach, error) {
	var httpEndpoint string
	for _, endpoint := range listenEndpoints {
		parsed, err := localipc.Parse(endpoint)
		if err != nil {
			continue
		}
		switch parsed.Scheme {
		case "unix":
			// npipe is deliberately absent: a Windows named pipe is not a
			// filesystem object a Linux container can bind, and the Windows
			// backend is wslc, which brings its own relay.
			if localSourceBindSupported(daemonHost) {
				return controlPlaneReach{
					url:       "unix://" + parsed.Value,
					socketDir: filepath.Dir(parsed.Value),
				}, nil
			}
		case "http":
			if httpEndpoint == "" {
				httpEndpoint = parsed.Value
			}
		}
	}
	if httpEndpoint != "" {
		return controlPlaneReach{url: hostGatewayURL(httpEndpoint)}, nil
	}
	return controlPlaneReach{}, fmt.Errorf(
		"pool containers on Docker daemon %q cannot reach this control plane: it listens on %s, none of which they can dial. "+
			"Set the provider's controlPlaneUrl, or add an HTTP endpoint to DISCOBOX_SERVER_LISTEN",
		daemonHost, strings.Join(listenEndpoints, ", "))
}

// hostGatewayURL rewrites an HTTP listen address into one a container resolves,
// keeping its port. A wildcard or loopback bind means nothing inside a
// container, which reaches the host through the gateway alias instead.
func hostGatewayURL(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	port := parsed.Port()
	if port == "" {
		port = strconv.Itoa(controlplane.DefaultPort)
	}
	host := parsed.Hostname()
	switch host {
	case "", "0.0.0.0", "::", "127.0.0.1", "localhost", "[::1]", "::1":
		host = dockerHostGateway
	}
	return "http://" + net.JoinHostPort(host, port)
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
