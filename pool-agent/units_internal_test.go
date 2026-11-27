package poolagent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pool-agent/poolunits"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
)

// systemd holds the agent's own listener where it can bind it, a TCP address
// of a literal IP or a Unix socket, and nothing else: a VSOCK agent binds its
// own.
func TestTheIngressSocketIsHeldWhereSystemdCanBindIt(t *testing.T) {
	for raw, want := range map[string]string{
		"http://0.0.0.0:3002":       "0.0.0.0:3002",
		"http://:3002":              "3002",
		"https://10.0.0.4:443":      "10.0.0.4:443",
		"unix:///run/agent.sock":    "/run/agent.sock",
		"http://pool.internal:3002": "",
		"vsock://:3002":             "",
		"vsock+yamux://:3002":       "",
		"":                          "",
	} {
		got, ok := socketListen(raw)
		if got != want || ok != (want != "") {
			t.Errorf("socketListen(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
}

// A pool's sockets are each its agent's, proxy's and mediator's listeners; a
// pool reached over a channel has its control-plane socket, and one behind an
// edge its tunnels.
func TestThePoolsSocketsAreWhatItServes(t *testing.T) {
	root := layout.ContainerAt(t.TempDir())
	names := func(sockets []poolunits.Socket) []string {
		var got []string
		for _, socket := range sockets {
			got = append(got, socket.Name)
		}
		return got
	}
	direct := names(poolSockets(root, Bootstrap{AgentListenURL: "http://0.0.0.0:3002", ProjectID: "prj", PoolID: "pool"}, false))
	for _, want := range []string{poolunits.IngressSocket, poolunits.OriginsSocket, poolunits.AssetsSocket, poolunits.ProxySocket, poolunits.CredentialsSocket, poolunits.DNSSocket, poolunits.ProxyControlSocket, poolunits.MediatorSocket, poolunits.RegistryFrontSocket} {
		if !slices.Contains(direct, want) {
			t.Errorf("a pool's sockets %v lack %s", direct, want)
		}
	}
	if slices.Contains(direct, poolunits.ControlPlaneSocket) || slices.Contains(direct, poolunits.TunnelsSocket) {
		t.Errorf("a pool that dials its control plane, behind no edge, holds %v", direct)
	}
	channel := names(poolSockets(root, Bootstrap{ControlPlaneChannel: true, ProjectID: "prj", PoolID: "pool"}, true))
	if !slices.Contains(channel, poolunits.ControlPlaneSocket) || !slices.Contains(channel, poolunits.TunnelsSocket) || slices.Contains(channel, poolunits.IngressSocket) {
		t.Errorf("a channel pool behind an edge, its agent on no listener systemd binds, holds %v", channel)
	}
}

// A Docker pool serves tunnels when its environment names an edge, read and
// checked as the proxy reads it; a machine pool when it is reached only
// through its edge.
func TestTunnelsAreServedWhereTheProxyServesThem(t *testing.T) {
	env := filepath.Join(t.TempDir(), "environment")
	if served, err := tunnelsServed(Bootstrap{}, env); served || err != nil {
		t.Fatalf("with no environment file: %v, %v", served, err)
	}
	if err := os.WriteFile(env, []byte(proxyagent.EnvTunnelURL+`="wss://edge.example.com"`+"\nOTHER=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if served, err := tunnelsServed(Bootstrap{}, env); !served || err != nil {
		t.Fatalf("with an edge named: %v, %v", served, err)
	}
	if err := os.WriteFile(env, []byte(proxyagent.EnvTunnelURL+"=http://edge.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tunnelsServed(Bootstrap{}, env); err == nil {
		t.Fatal("a tunnel URL the proxy refuses was taken")
	}
	machine := Bootstrap{Runtime: RuntimeMachine, PoolURL: "https://pool.example.com", IngressURL: "https://pool.example.com"}
	if served, err := tunnelsServed(machine, env); !served || err != nil {
		t.Fatalf("a machine pool behind its edge: %v, %v", served, err)
	}
}
