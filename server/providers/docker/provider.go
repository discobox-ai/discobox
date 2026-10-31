// Package docker registers the "docker" provider type: pools run as
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
	"path"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/pkg/controlplane"
	"github.com/discobox-ai/discobox/pkg/endpoint"
	"github.com/discobox-ai/discobox/pkg/wire"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

const (
	ProviderType      = "docker"
	defaultAgentPort  = 3002
	dockerHostGateway = "host.docker.internal"
	dockerSocketPath  = "/var/run/docker.sock"
	labelProviderType = "discobox.provider_type"
)

// DefaultAgentPort returns the default pool-agent port exposed by Docker pools.
func DefaultAgentPort() int { return defaultAgentPort }

// Config is the persisted provider instance configuration.
type Config struct {
	poolruntime.PoolPolicy

	ControlPlaneURL  string                   `json:"controlPlaneUrl,omitempty"`
	Host             string                   `json:"host,omitempty"`
	SandboxBaseImage string                   `json:"sandboxBaseImage,omitempty"`
	Network          string                   `json:"network,omitempty"`
	AgentPort        int                      `json:"agentPort,omitempty"`
	Privileged       *bool                    `json:"privileged,omitempty"`
	CgroupNSMode     string                   `json:"cgroupNsMode,omitempty"`
	DockerSocket     string                   `json:"bindDockerSocket,omitempty"`
	HostMounts       []dockerworker.HostMount `json:"hostMounts,omitempty"`
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

func FactoryWithPoolManager(poolManager poolruntime.PoolManager, imageSync *dockerworker.DevelopmentImageSynchronizer, listenEndpoints []string, serverDefaults dockerworker.ServerDefaults) sandbox.ProviderFactory {
	return func(ctx context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return newFromInstance(ctx, instance, poolManager, imageSync, listenEndpoints, serverDefaults)
	}
}

func newFromInstance(ctx context.Context, instance *model.SandboxProviderInstance, poolManager poolruntime.PoolManager, imageSync *dockerworker.DevelopmentImageSynchronizer, listenEndpoints []string, serverDefaults dockerworker.ServerDefaults) (sandbox.Provider, error) {
	cfg, err := Decode(instance.Config)
	if err != nil {
		return nil, err
	}
	driver, err := NewLocalDriver(ctx, cfg.Host, effectiveAgentPort(cfg.AgentPort))
	if err != nil {
		return nil, err
	}
	engineCfg, err := engineConfig(cfg, listenEndpoints, driver.DaemonHost(), serverDefaults)
	if err != nil {
		_ = driver.Close()
		return nil, err
	}
	engineCfg.DevelopmentImageSync = imageSync
	engineCfg.ProgressReporter = sandbox.PoolProgressReporterFor(poolManager)
	engine, err := dockerworker.New(engineCfg, driver)
	if err != nil {
		_ = driver.Close()
		return nil, err
	}
	if err := startBackgroundWatchers(driver, engine, poolManager, instance); err != nil {
		_ = engine.Close()
		return nil, err
	}
	return poolruntime.New(engine, Definition(), poolManager, nil), nil
}

// daemonIsLocal reports whether the Docker daemon at daemonHost runs on this
// machine — which decides two separate things: whether the control plane's own
// Unix socket is reachable from a pool container, and whether this process can
// read the daemon's journal.
//
// Only socket transports qualify. A socket means the daemon is on this machine,
// including Docker Desktop, which shares host paths into its VM transparently.
// ssh:// and remote tcp:// daemons cannot see these files at all, and a
// tcp://localhost daemon may be forwarded anywhere, so none of them qualify.
//
// The rule itself is the engine's, because the image store asks the same
// question of the same hosts (ADR 0113 §4) and two answers would eventually
// disagree.
func daemonIsLocal(daemonHost string) bool {
	return dockerworker.DaemonLocalityForHost(daemonHost) == dockerworker.DaemonOnThisMachine
}

// engineConfig maps the docker provider configuration to the shared engine
// configuration, deriving how the pool agent reaches the control plane when the
// instance does not name it.
func engineConfig(cfg Config, listenEndpoints []string, daemonHost string, serverDefaults dockerworker.ServerDefaults) (dockerworker.Config, error) {
	engineCfg := dockerworker.Config{
		ControlPlaneURL:     strings.TrimSpace(cfg.ControlPlaneURL),
		SandboxBaseImage:    dockerworker.EffectiveSandboxBaseImage(cfg.SandboxBaseImage, serverDefaults),
		ImageRetention:      serverDefaults.ImageRetention,
		ImageCache:          serverDefaults.ImageCache,
		Network:             cfg.Network,
		AgentPort:           effectiveAgentPort(cfg.AgentPort),
		Privileged:          cfg.Privileged,
		CgroupNSMode:        cfg.CgroupNSMode,
		DockerSocket:        cfg.DockerSocket,
		HostMounts:          cfg.HostMounts,
		Labels:              map[string]string{labelProviderType: ProviderType},
		ProxyAuditRetention: cfg.ProxyAuditRetention.Value(),
		AuditSpoolBudget:    cfg.AuditSpoolBudget,
		SandboxIdleTimeout:  cfg.SandboxIdleTimeout.Value(),
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
	for _, raw := range listenEndpoints {
		parsed, err := endpoint.Parse(raw)
		if err != nil {
			continue
		}
		switch parsed.Scheme {
		case "unix":
			// npipe is deliberately absent: a Windows named pipe is not a
			// filesystem object a Linux container can bind, and the Windows
			// backend is wslc, which brings its own relay.
			if daemonIsLocal(daemonHost) {
				return controlPlaneReach{
					url:       wire.UnixURL(parsed.Value),
					socketDir: path.Dir(parsed.Value),
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

func controlPlaneURLUsesHostGateway(value string) bool {
	return strings.Contains(value, "://"+dockerHostGateway) || strings.HasPrefix(value, dockerHostGateway+":")
}

// Definition describes the Docker provider for provider catalogs.
func Definition() sandbox.ProviderDefinition {
	return sandbox.ProviderDefinition{
		Name:        "Docker",
		Icon:        "docker",
		Description: "Runs VM-style workers as Docker containers with systemd as PID 1.",
		ConfigFields: append([]sandbox.ProviderConfigField{
			{Key: "controlPlaneUrl", Label: "Control Plane URL", Type: "string", Placeholder: controlplane.DefaultURL(dockerHostGateway, controlplane.DefaultPort), Advanced: true},
			{Key: "host", Label: "Docker Host", Type: "string", Advanced: true},
			{Key: "sandboxBaseImage", Label: "Sandbox Base Image", Type: "string", Placeholder: dockerworker.DefaultSandboxBaseImage, Description: "Sandbox base image every sandbox delivered its platform runs, and the pool's own container runs, provisioned as a pool (ADR 26-10-16-423 §§7, 10, ADR 26-10-24-953 §2).", Advanced: true},
			{Key: "network", Label: "Docker Network", Type: "string", Advanced: true},
			{Key: "privileged", Label: "Privileged", Type: "boolean", Advanced: true},
			{Key: "cgroupNsMode", Label: "Cgroup Namespace", Type: "string", Advanced: true},
			{Key: "bindDockerSocket", Label: "Bind Docker Socket", Type: "string", Placeholder: dockerSocketPath, Advanced: true},
			{Key: "agentPort", Label: "Harness Port", Type: "number", Placeholder: strconv.Itoa(defaultAgentPort), Advanced: true},
		}, poolruntime.PoolPolicyConfigFields()...),
	}
}
