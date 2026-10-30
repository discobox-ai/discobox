package poolagent

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
)

// Hop forms, as a machine pool's runtime condition names them.
const (
	hopDirect = "direct"
	hopEdge   = "edge"
)

// poolHop is how this pool and its sandboxes reach each other, and all its
// sandboxes reach (ADR 26-10-29-481): the same services on every runtime,
// named by discobox's own names, at an address the runtime gives.
type poolHop struct {
	// endpoints is where the pool serves its sandboxes, as their bootstraps
	// name it. The scheme says the transport: https for a direct path, wss
	// for a tunnel through an edge.
	endpoints sandboxconfig.PoolEndpoints
	// tunnelURL is the wss base of the edge the pool's proxy unit serves
	// tunnels for, and empty for a pool its sandboxes reach directly.
	tunnelURL string
	// egress is what a sandbox's machine may reach beyond the pool's network,
	// and nil on a Docker pool, whose sandbox network has no route out.
	egress *machine.Egress
	// agentTLS is whether the pool's sandboxes' agents serve their API over
	// TLS, which the pool tells each in its bootstrap: the pool's setting,
	// and never behind an edge, which speaks HTTP to them (§3).
	agentTLS bool
	// form is hopDirect or hopEdge on a machine pool, and empty on a Docker
	// pool.
	form string
}

// resolvePoolHop is the hop bootstrap describes. A Docker pool's sandboxes
// reach it by its name on the pool's network, or through the edge its
// environment names (ADR 26-10-23-719); a machine pool's reach it at its
// private URL.
func resolvePoolHop(bootstrap Bootstrap) (poolHop, error) {
	agentTLS := !bootstrap.DisableAgentEndpointTLS
	if bootstrap.EffectiveRuntime() != RuntimeMachine {
		tunnelURL, err := proxyagent.ConfiguredTunnelURL()
		if err != nil {
			return poolHop{}, err
		}
		return poolHop{endpoints: proxyagent.PoolEndpoints(tunnelURL), tunnelURL: tunnelURL, agentTLS: agentTLS}, nil
	}
	public, err := url.Parse(bootstrap.PublicURL)
	if err != nil {
		return poolHop{}, fmt.Errorf("the pool's public URL: %w", err)
	}
	private, err := url.Parse(bootstrap.PrivateURL)
	if err != nil {
		return poolHop{}, fmt.Errorf("the pool's private URL: %w", err)
	}
	if !bootstrap.reachedThroughEdge() {
		// The pool's own name, as on a Docker pool, and discobox's answer for
		// it: the provider's host, which the sandbox resolves through its
		// provider at each dial, so an address that moves is followed (§2).
		endpoints := proxyagent.PoolEndpoints("")
		endpoints.Address = private.Hostname()
		return poolHop{endpoints: endpoints, egress: &machine.Egress{}, agentTLS: agentTLS, form: hopDirect}, nil
	}
	// No path between the driver's machines but the public URL, an edge: the
	// sandboxes tunnel through it, with their mTLS inside, and reach nothing
	// else on the internet.
	if public.Scheme != "https" {
		return poolHop{}, fmt.Errorf("the pool's machine is reached only at %s, which is not https, so no tunnel can be carried through it", bootstrap.PublicURL)
	}
	tunnelURL := (&url.URL{Scheme: "wss", Host: public.Host}).String()
	if err := proxyagent.ValidateTunnelURL(tunnelURL); err != nil {
		return poolHop{}, err
	}
	return poolHop{
		endpoints: proxyagent.PoolEndpoints(tunnelURL),
		tunnelURL: tunnelURL,
		egress:    &machine.Egress{Allow: []string{public.Hostname()}},
		form:      hopEdge,
	}, nil
}

// requireMachineFence refuses a driver that cannot hold a sandbox's machine
// to its pool: one that cannot isolate a machine from the rest of its
// provider's account, or cannot fence its egress (ADR 26-10-29-481 §4).
func requireMachineFence(driver string, caps machine.Capabilities) error {
	var missing []error
	if !caps.Isolation {
		missing = append(missing, errors.New("cannot isolate a sandbox's machine from the rest of its provider's account"))
	}
	if !caps.Egress {
		missing = append(missing, errors.New("cannot fence a sandbox's machine's egress to its pool"))
	}
	if len(missing) > 0 {
		return fmt.Errorf("the %s driver cannot carry a machine pool: %w", driver, errors.Join(missing...))
	}
	return nil
}
