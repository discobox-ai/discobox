package poolagent

import (
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
		{"BuildKit mediator", buildkitagent.MediatorListen},
		{"build registry", buildkitagent.RegistryListen},
	}
	seen := make(map[string]string, len(listeners))
	for _, l := range listeners {
		if other, ok := seen[l.addr]; ok {
			t.Errorf("%s and %s both listen on %s", other, l.owner, l.addr)
			continue
		}
		seen[l.addr] = l.owner
	}
}
