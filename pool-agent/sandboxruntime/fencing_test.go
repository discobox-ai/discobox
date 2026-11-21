package sandboxruntime

import (
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

// A driver carries a pool on its networks, as boxd does, or fenced at its
// provider's edge, as archil does, and a driver that does neither is refused,
// saying what it lacks (ADR 26-11-11-963 §5, ADR 26-11-16-619 §1).
func TestPoolFencing(t *testing.T) {
	allow := []machine.EgressFence{machine.EgressNone, machine.EgressAllow}
	closeDNS := []machine.EgressFence{machine.EgressNone, machine.EgressAllow, machine.EgressCloseDNS}
	credentialed := machine.URLCapabilities{Ingress: machine.URLOptional, Pool: machine.URLAlways, Credential: true}
	for _, tc := range []struct {
		name string
		caps machine.Capabilities
		want Fencing
		// refused is what the refusal says, when there is one.
		refused []string
	}{
		{name: "boxd", caps: machine.Capabilities{Bare: machine.BareStrip, Networks: true, Egress: allow}, want: FencedOnNetwork},
		{name: "networks and an edge", caps: machine.Capabilities{Bare: machine.BareStrip, Networks: true, Egress: closeDNS, URLs: credentialed}, want: FencedOnNetwork},
		{name: "archil", caps: machine.Capabilities{Bare: machine.BareAlways, Egress: closeDNS, URLs: credentialed}, want: FencedAtEdge},
		{name: "a stripping edge", caps: machine.Capabilities{Bare: machine.BareStrip, Egress: closeDNS, URLs: credentialed}, want: FencedAtEdge},
		{name: "archil before its fence closed DNS", caps: machine.Capabilities{Bare: machine.BareAlways, Egress: allow, URLs: credentialed}, refused: []string{"network of its pool's", "no resolver closed"}},
		{name: "an exposed edge", caps: machine.Capabilities{Bare: machine.BareAlways, Egress: closeDNS, URLs: machine.URLCapabilities{Ingress: machine.URLAlways, Pool: machine.URLAlways}}, refused: []string{"network of its pool's", "only a credential"}},
		{name: "networks without stripping", caps: machine.Capabilities{Networks: true, Egress: allow}, refused: []string{"nothing of its provider's account"}},
		{name: "an edge without stripping", caps: machine.Capabilities{Egress: closeDNS, URLs: credentialed}, refused: []string{"nothing of its provider's account"}},
		{name: "no egress fence", caps: machine.Capabilities{Bare: machine.BareStrip, Networks: true}, refused: []string{"cannot fence a sandbox's machine's egress"}},
		{name: "nothing", refused: []string{"nothing of its provider's account", "cannot fence a sandbox's machine's egress"}},
	} {
		got, err := PoolFencing("fake", tc.caps)
		if tc.refused == nil {
			if err != nil || got != tc.want {
				t.Errorf("%s: PoolFencing = %q, %v; want %q", tc.name, got, err, tc.want)
			}
			continue
		}
		if err == nil || got != "" || !strings.Contains(err.Error(), "the fake driver cannot carry a machine pool") {
			t.Errorf("%s: PoolFencing = %q, %v; want it refused", tc.name, got, err)
			continue
		}
		for _, want := range tc.refused {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: PoolFencing = %v, want it to say %q", tc.name, err, want)
			}
		}
	}
}
