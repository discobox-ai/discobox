package poolagent

import (
	"net"
	"net/url"
	"testing"

	"github.com/discobox-ai/discobox/pool-agent/buildkitagent"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
)

// TestPoolListenAddressesAreDistinct guards the pool container's port map.
// Every listener below runs in the pool container's single network namespace,
// but they are split across systemd units, so a duplicate port is not a
// compile error — it is a crash-looping unit that only shows up at runtime,
// and whichever unit starts second stays down for the life of the pool.
func TestPoolListenAddressesAreDistinct(t *testing.T) {
	listeners := []struct {
		owner string
		addr  string
	}{
		{"pool proxy", proxyagent.ListenAddress},
		{"agent credentials endpoint", proxyagent.CredentialsListenAddress},
		{"sandbox DNS", proxyagent.DNSListenAddress},
		{"proxy control API", proxyagent.ControlListenAddress},
		{"BuildKit mediator", buildkitagent.MediatorListen},
		{"build registry", buildkitagent.RegistryListen},
		{"pool tunnels", proxyagent.TunnelListenAddress},
	}
	// Keyed by port, not by address: the proxy control API binds loopback while
	// the rest bind every interface, and 127.0.0.1:N and 0.0.0.0:N collide in
	// one namespace even though the two strings differ.
	seen := make(map[string]string, len(listeners))
	for _, l := range listeners {
		_, port, err := net.SplitHostPort(l.addr)
		if err != nil {
			t.Fatalf("%s listen address %q: %v", l.owner, l.addr, err)
		}
		if other, ok := seen[port]; ok {
			t.Errorf("%s and %s both listen on port %s", other, l.owner, port)
			continue
		}
		seen[port] = l.owner
	}
}

// The sandbox's bootstrap names the mediator by a port proxyagent spells
// itself, and the tunnel splices onto that port too; both have to be the one
// the mediator unit binds.
func TestBootstrapNamesTheMediatorsPort(t *testing.T) {
	named, err := url.Parse(proxyagent.BuildkitMediatorURL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(buildkitagent.MediatorListen)
	if err != nil {
		t.Fatal(err)
	}
	if named.Port() != port {
		t.Fatalf("BuildkitMediatorURL names port %s, the mediator binds %s", named.Port(), port)
	}
}
