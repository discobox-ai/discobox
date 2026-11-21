package poolagent

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

func machineBootstrap(ingress, pool string) Bootstrap {
	return Bootstrap{
		ControlPlaneURL: "http://control.example",
		PoolID:          "pool-1",
		Runtime:         RuntimeMachine,
		Driver:          "boxd",
		MachinePool:     "pool-1",
		IngressURL:      ingress,
		PoolURL:         pool,
	}
}

// A Docker pool's sandboxes reach it by its name on the pool's network, and
// its network fences them: the hop names no fence and no answer for the name.
// Its agents serve TLS unless the pool disables it.
func TestPoolHopOfADockerPoolIsItsName(t *testing.T) {
	t.Setenv(proxyagent.EnvTunnelURL, "")
	bootstrap := Bootstrap{ControlPlaneURL: "http://control.example", PoolID: "pool-1"}
	hop, err := resolvePoolHop(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if hop.endpoints != proxyagent.PoolEndpoints("") || hop.tunnelURL != "" || hop.egress != nil || hop.form != "" || !hop.agentTLS {
		t.Fatalf("hop = %+v, want the pool's name, no fence, and TLS to its agents", hop)
	}
	bootstrap.DisableAgentEndpointTLS = true
	if hop, err := resolvePoolHop(bootstrap); err != nil || hop.agentTLS {
		t.Fatalf("hop of a pool that disables TLS = %+v, %v; want plain HTTP to its agents", hop, err)
	}
}

// A machine pool reached at a pool URL its ingress one is not is reached
// directly, by its own name, with the provider's host as discobox's answer
// for it, and its sandboxes' machines reach nothing beyond its network (ADR
// 26-10-29-481 §§2, 4).
func TestPoolHopOfAMachinePoolOnANetworkIsDirect(t *testing.T) {
	hop, err := resolvePoolHop(machineBootstrap("https://pool-1.boxd.sh", "http://pool-1.boxd:7302"))
	if err != nil {
		t.Fatal(err)
	}
	if !hop.endpoints.DNSSystemResolver {
		t.Fatal("machine DNS does not use stub")
	}
	if hop.form != hopDirect || hop.tunnelURL != "" || !hop.agentTLS {
		t.Fatalf("hop = %+v, want a direct one with no tunnel and TLS to its agents", hop)
	}
	want := proxyagent.PoolEndpoints("")
	want.Address = "pool-1.boxd"
	want.DNSSystemResolver = true
	if hop.endpoints != want {
		t.Fatalf("endpoints = %+v, want the pool's name, answered by its provider host", hop.endpoints)
	}
	if hop.egress == nil || len(hop.egress.Allow) != 0 {
		t.Fatalf("fence = %+v, want nothing beyond the pool's network", hop.egress)
	}
}

// A machine pool whose pool URL is its ingress URL, an edge, is tunneled to
// through it, its sandboxes' machines reach that host alone, and its agents
// serve plain HTTP, which is all an edge speaks to them, whatever the pool
// sets.
func TestPoolHopOfAMachinePoolBehindAnEdgeIsATunnel(t *testing.T) {
	hop, err := resolvePoolHop(machineBootstrap("https://pool-1.boxd.sh", "https://pool-1.boxd.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !hop.endpoints.DNSSystemResolver {
		t.Fatal("edge machine DNS does not use stub")
	}
	if hop.form != hopEdge || hop.tunnelURL != "wss://pool-1.boxd.sh" || hop.agentTLS {
		t.Fatalf("hop = %+v, want a tunnel through the edge and plain HTTP to its agents", hop)
	}
	want := proxyagent.PoolEndpoints("wss://pool-1.boxd.sh")
	want.DNSSystemResolver = true
	if hop.endpoints != want {
		t.Fatalf("endpoints = %+v, want the edge's tunnels", hop.endpoints)
	}
	if hop.egress == nil || len(hop.egress.Allow) != 1 || hop.egress.Allow[0] != "pool-1.boxd.sh" {
		t.Fatalf("fence = %+v, want the edge's host alone", hop.egress)
	}
}

// A tunnel needs TLS to the edge, so an ingress URL that is not https and is
// the only path carries no machine pool.
func TestPoolHopRefusesAPlainEdge(t *testing.T) {
	_, err := resolvePoolHop(machineBootstrap("http://127.0.0.1:7302", "http://127.0.0.1:7302"))
	if err == nil || !strings.Contains(err.Error(), "not https") {
		t.Fatalf("hop over a plain edge = %v, want it refused", err)
	}
}

// A driver that fences on networks keeps the hop it resolved. One that
// fences at its provider's edge needs the edge's hop, and closes DNS in the
// sandboxes' fence, leaving the hop's own fence as it was (ADR 26-11-16-619
// §§1, 3).
func TestPoolHopFencedByTheDriver(t *testing.T) {
	direct, err := resolvePoolHop(machineBootstrap("https://pool-1.boxd.sh", "http://pool-1.boxd:7302"))
	if err != nil {
		t.Fatal(err)
	}
	if hop, err := direct.fencedBy("boxd", sandboxruntime.FencedOnNetwork); err != nil || hop.egress != direct.egress || hop.form != hopDirect {
		t.Fatalf("a direct hop on networks = %+v, %v; want it as resolved", hop, err)
	}
	if _, err := direct.fencedBy("archil", sandboxruntime.FencedAtEdge); err == nil || !strings.Contains(err.Error(), "not reached through one") {
		t.Fatalf("a direct hop at an edge = %v, want it refused", err)
	}
	edge, err := resolvePoolHop(machineBootstrap("https://7302-r.z.archil.app", "https://7302-r.z.archil.app"))
	if err != nil {
		t.Fatal(err)
	}
	hop, err := edge.fencedBy("archil", sandboxruntime.FencedAtEdge)
	if err != nil {
		t.Fatal(err)
	}
	if hop.form != hopEdge || hop.egress == nil || !hop.egress.CloseDNS || len(hop.egress.Allow) != 1 || hop.egress.Allow[0] != "7302-r.z.archil.app" {
		t.Fatalf("an edge hop at an edge = %+v, want the edge's host alone with DNS closed", hop)
	}
	if edge.egress.CloseDNS {
		t.Fatal("fencing the hop changed the fence it was fenced from")
	}
	if hop, err := edge.fencedBy("boxd", sandboxruntime.FencedOnNetwork); err != nil || hop.egress.CloseDNS {
		t.Fatalf("an edge hop on networks = %+v, %v; want it as resolved", hop, err)
	}
}
