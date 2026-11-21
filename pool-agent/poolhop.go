package poolagent

import (
	"fmt"
	"net/url"

	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
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
// pool URL.
func resolvePoolHop(bootstrap Bootstrap) (poolHop, error) {
	agentTLS := !bootstrap.DisableAgentEndpointTLS
	if bootstrap.EffectiveRuntime() != RuntimeMachine {
		tunnelURL, err := proxyagent.ConfiguredTunnelURL()
		if err != nil {
			return poolHop{}, err
		}
		return poolHop{endpoints: proxyagent.PoolEndpoints(tunnelURL), tunnelURL: tunnelURL, agentTLS: agentTLS}, nil
	}
	pool, err := url.Parse(bootstrap.PoolURL)
	if err != nil {
		return poolHop{}, fmt.Errorf("the pool's pool URL: %w", err)
	}
	if !bootstrap.reachedThroughEdge() {
		// The pool's own name, as on a Docker pool, and discobox's answer for
		// it: the provider's host, which the sandbox resolves through its
		// provider at each dial, so an address that moves is followed (§2).
		endpoints := proxyagent.PoolEndpoints("")
		endpoints.Address = pool.Hostname()
		endpoints.DNSSystemResolver = true
		return poolHop{endpoints: endpoints, egress: &machine.Egress{}, agentTLS: agentTLS, form: hopDirect}, nil
	}
	// A pool URL that is the ingress URL is an edge: the driver gives its
	// machines no path to each other but that one. The sandboxes tunnel
	// through their pool URL, with their mTLS inside, and reach nothing else
	// beyond the pool's network. The ingress URL only tells the form.
	if pool.Scheme != "https" {
		return poolHop{}, fmt.Errorf("the pool's machine is reached only at %s, which is not https, so no tunnel can be carried through it", bootstrap.PoolURL)
	}
	tunnelURL := (&url.URL{Scheme: "wss", Host: pool.Host}).String()
	if err := proxyagent.ValidateTunnelURL(tunnelURL); err != nil {
		return poolHop{}, err
	}
	endpoints := proxyagent.PoolEndpoints(tunnelURL)
	endpoints.DNSSystemResolver = true
	return poolHop{
		endpoints: endpoints,
		tunnelURL: tunnelURL,
		egress:    &machine.Egress{Allow: []string{pool.Hostname()}},
		form:      hopEdge,
	}, nil
}

// fencedBy is the hop as a driver that fences its pool's sandboxes so
// holds them (sandboxruntime.PoolFencing). On its networks, the hop is as
// resolved. At its provider's edge, the sandboxes have no network to reach
// the pool on, so the hop must be the edge's, and their fence closes DNS
// too, so no resolver answers them (ADR 26-11-16-619 §1). Nothing yet gives
// them the address 619 §3 has them dial in place of the pool's host: the hop
// still names the host, which their bridges cannot resolve.
func (h poolHop) fencedBy(driver string, fencing sandboxruntime.Fencing) (poolHop, error) {
	if fencing != sandboxruntime.FencedAtEdge {
		return h, nil
	}
	if h.form != hopEdge {
		return poolHop{}, fmt.Errorf("the %s driver fences a sandbox's machine at its provider's edge, but the pool's machine is not reached through one (its pool URL is not its ingress URL), so its sandboxes could not reach it", driver)
	}
	egress := *h.egress
	egress.CloseDNS = true
	h.egress = &egress
	return h, nil
}
