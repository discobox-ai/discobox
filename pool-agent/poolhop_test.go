package poolagent

import (
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
)

func machineBootstrap(public, private string) Bootstrap {
	return Bootstrap{
		ControlPlaneURL: "http://control.example",
		PoolID:          "pool-1",
		Runtime:         RuntimeMachine,
		Driver:          "boxd",
		MachineProject:  "pool-1",
		PublicURL:       public,
		PrivateURL:      private,
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

// A machine pool reached at a private URL its public one is not is reached
// directly, by its own name, with the provider's host as discobox's answer
// for it, and its sandboxes' machines reach nothing beyond its network (ADR
// 26-10-29-481 §§2, 4).
func TestPoolHopOfAMachinePoolOnANetworkIsDirect(t *testing.T) {
	hop, err := resolvePoolHop(machineBootstrap("https://pool-1.boxd.sh", "http://pool-1.boxd:7302"))
	if err != nil {
		t.Fatal(err)
	}
	if hop.form != hopDirect || hop.tunnelURL != "" || !hop.agentTLS {
		t.Fatalf("hop = %+v, want a direct one with no tunnel and TLS to its agents", hop)
	}
	want := proxyagent.PoolEndpoints("")
	want.Address = "pool-1.boxd"
	if hop.endpoints != want {
		t.Fatalf("endpoints = %+v, want the pool's name, answered by its provider host", hop.endpoints)
	}
	if hop.egress == nil || len(hop.egress.Allow) != 0 {
		t.Fatalf("fence = %+v, want nothing beyond the pool's network", hop.egress)
	}
}

// A machine pool reached only at its public URL, an edge, is tunnelled to
// through it, its sandboxes' machines reach that host alone, and its agents
// serve plain HTTP, which is all an edge speaks to them, whatever the pool
// sets.
func TestPoolHopOfAMachinePoolBehindAnEdgeIsATunnel(t *testing.T) {
	hop, err := resolvePoolHop(machineBootstrap("https://pool-1.boxd.sh", "https://pool-1.boxd.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if hop.form != hopEdge || hop.tunnelURL != "wss://pool-1.boxd.sh" || hop.agentTLS {
		t.Fatalf("hop = %+v, want a tunnel through the edge and plain HTTP to its agents", hop)
	}
	if hop.endpoints != proxyagent.PoolEndpoints("wss://pool-1.boxd.sh") {
		t.Fatalf("endpoints = %+v, want the edge's tunnels", hop.endpoints)
	}
	if hop.egress == nil || len(hop.egress.Allow) != 1 || hop.egress.Allow[0] != "pool-1.boxd.sh" {
		t.Fatalf("fence = %+v, want the edge's host alone", hop.egress)
	}
}

// A tunnel needs TLS to the edge, so a public URL that is not https and is
// the only path carries no machine pool.
func TestPoolHopRefusesAPlainEdge(t *testing.T) {
	_, err := resolvePoolHop(machineBootstrap("http://127.0.0.1:7302", "http://127.0.0.1:7302"))
	if err == nil || !strings.Contains(err.Error(), "not https") {
		t.Fatalf("hop over a plain edge = %v, want it refused", err)
	}
}

func TestRequireMachineFence(t *testing.T) {
	if err := requireMachineFence("boxd", machine.Capabilities{Isolation: true, Egress: true}); err != nil {
		t.Fatalf("a driver that isolates and fences: %v", err)
	}
	for _, tc := range []struct {
		name string
		caps machine.Capabilities
		want []string
	}{
		{name: "no isolation", caps: machine.Capabilities{Egress: true}, want: []string{"cannot isolate"}},
		{name: "no fence", caps: machine.Capabilities{Isolation: true}, want: []string{"cannot fence"}},
		{name: "neither", want: []string{"cannot isolate", "cannot fence"}},
	} {
		err := requireMachineFence("fake", tc.caps)
		for _, want := range tc.want {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: requireMachineFence = %v, want it to say %q", tc.name, err, want)
			}
		}
	}
}
