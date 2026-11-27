package poolagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"

	"github.com/joho/godotenv"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pkg/pooloutbound"
	"github.com/discobox-ai/discobox/pkg/wire"
	"github.com/discobox-ai/discobox/pool-agent/buildkitagent"
	poolbundle "github.com/discobox-ai/discobox/pool-agent/bundle"
	"github.com/discobox-ai/discobox/pool-agent/poolunits"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
)

// UnitsCommand is the agent binary's subcommand that keeps the pool's box
// running the agent and proxy it holds (poolunits): `units apply`, run by the
// pool bundle and by the pool's maker after each apply, and `units start`,
// run by a socket that finds nothing serving it.
const UnitsCommand = "units"

// unitsStateFile records the slot the box runs and what it started with.
const unitsStateFile = "/var/lib/discobox/pool-units.json"

// RunUnits runs `discobox-pool-agent units <apply|start>`.
func RunUnits(ctx context.Context, logger *slog.Logger, args []string) error {
	if len(args) != 1 || (args[0] != "apply" && args[0] != "start") {
		return errors.New("usage: discobox-pool-agent units apply|start")
	}
	root := layout.Container()
	cfg := poolunits.Config{
		SystemdDir: root.System("/etc/systemd/system"),
		StateFile:  root.System(unitsStateFile),
		Binary:     root.System(poolbundle.AgentBinary),
		Inputs: []string{
			root.System(poolbundle.BootstrapFile),
			root.System(poolbundle.EnvironmentFile),
			root.System(pooloutbound.RuntimeFile),
		},
		Booted:    booted(root),
		Systemctl: poolunits.ExecSystemctl,
		Logger:    logger,
	}
	if args[0] == "start" {
		return poolunits.Start(ctx, cfg)
	}
	if !cfg.Booted {
		// A bundle applied in an image's build: nothing runs yet, and no
		// bootstrap says what the sockets are. The maker's apply in the
		// running box renders them.
		return nil
	}
	bootstrap, err := readBootstrapFile(root.System(poolbundle.BootstrapFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	tunnels, err := tunnelsServed(bootstrap, root.System(poolbundle.EnvironmentFile))
	if err != nil {
		return err
	}
	cfg.Sockets = poolSockets(root, bootstrap, tunnels)
	cfg.HandOver = os.Getenv(poolbundle.HandOverEnv) == "1"
	return poolunits.Apply(ctx, cfg)
}

// booted reports whether systemd runs this box.
func booted(root layout.Root) bool {
	info, err := os.Stat(root.System("/run/systemd/system"))
	return err == nil && info.IsDir()
}

// poolSockets are the sockets systemd holds for the pool bootstrap describes:
// each listener its agent, proxy and mediator serve. The agent's own listener
// is held only where systemd can bind it, on TCP or a Unix socket: an agent
// listening on VSOCK binds its own, and its pool restarts rather than hands
// over.
func poolSockets(root layout.Root, bootstrap Bootstrap, tunnels bool) []poolunits.Socket {
	var sockets []poolunits.Socket
	if listen, ok := socketListen(bootstrap.AgentListenURL); ok {
		sockets = append(sockets, poolunits.Socket{Name: poolunits.IngressSocket, Listen: listen, Role: poolunits.RoleAgent})
	}
	if bootstrap.ControlPlaneChannel {
		sockets = append(sockets, poolunits.Socket{Name: poolunits.ControlPlaneSocket, Listen: ControlPlaneChannelSocket, Role: poolunits.RoleAgent})
	}
	sockets = append(sockets,
		poolunits.Socket{Name: poolunits.OriginsSocket, Listen: proxyagent.OriginsListenAddress, Role: poolunits.RoleAgent},
		poolunits.Socket{Name: poolunits.AssetsSocket, Listen: root.ProxyAssetsSocket(bootstrap.ProjectID, bootstrap.PoolID), Role: poolunits.RoleAgent},
		poolunits.Socket{Name: poolunits.ProxySocket, Listen: proxyagent.ListenAddress, Role: poolunits.RoleProxy},
		poolunits.Socket{Name: poolunits.CredentialsSocket, Listen: proxyagent.CredentialsListenAddress, Role: poolunits.RoleProxy},
		poolunits.Socket{Name: poolunits.DNSSocket, Listen: proxyagent.DNSListenAddress, Role: poolunits.RoleProxy},
		poolunits.Socket{Name: poolunits.ProxyControlSocket, Listen: proxyagent.ControlListenAddress, Role: poolunits.RoleProxy},
	)
	if tunnels {
		sockets = append(sockets, poolunits.Socket{Name: poolunits.TunnelsSocket, Listen: proxyagent.TunnelListenAddress, Role: poolunits.RoleProxy})
	}
	return append(sockets,
		poolunits.Socket{Name: poolunits.MediatorSocket, Listen: buildkitagent.MediatorListen, Role: poolunits.RoleMediator},
		poolunits.Socket{Name: poolunits.RegistryFrontSocket, Listen: root.ProxyRegistrySocket(bootstrap.ProjectID, bootstrap.PoolID), Role: poolunits.RoleMediator},
	)
}

// socketListen is the ListenStream= of an agent listen URL systemd can bind:
// a TCP address of a literal IP, or a Unix socket's path.
func socketListen(raw string) (string, bool) {
	if strings.TrimSpace(raw) == "" {
		return "", false
	}
	endpoint, err := wire.Parse(raw)
	if err != nil {
		return "", false
	}
	switch endpoint.Scheme {
	case "http", "https":
		host, port, err := net.SplitHostPort(endpoint.Host)
		if err != nil {
			return "", false
		}
		if host == "" {
			return port, true
		}
		if _, err := netip.ParseAddr(host); err != nil {
			return "", false
		}
		return endpoint.Host, true
	case "unix":
		return endpoint.Path, endpoint.Path != ""
	default:
		return "", false
	}
}

// tunnelsServed reports whether the proxy serves an edge's tunnels: a machine
// pool reached only through its edge, or a pool whose environment names an
// edge (proxyagent.EnvTunnelURL), as resolvePoolHop decides.
func tunnelsServed(bootstrap Bootstrap, environmentFile string) (bool, error) {
	if bootstrap.EffectiveRuntime() == RuntimeMachine {
		return bootstrap.reachedThroughEdge(), nil
	}
	env, err := godotenv.Read(environmentFile)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", environmentFile, err)
	}
	tunnel, err := proxyagent.TunnelURL(env[proxyagent.EnvTunnelURL])
	return tunnel != "", err
}
