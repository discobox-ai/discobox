package poolagent

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

// routeTable is a /proc/net/route with a default route through 10.0.2.1
// (0102000A, little-endian) beside the link's own.
const routeTable = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	0002000A	00000000	0001	0	0	0	00FFFFFF	0	0	0
eth0	00000000	0102000A	0003	0	0	0	00000000	0	0	0
`

func writeRouteTable(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "route")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The bootstrap says how the pool's engine authenticates in one place: the
// driver's configuration turning on the field its definition's in-machine
// auth names. A driver that declares none is never in-machine.
func TestInMachineAuthIsTheDriverConfigurations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		driver   string
		config   string
		declared bool
		on       bool
		wantErr  string
	}{
		{name: "boxd with its field on", driver: "boxd", config: `{"inMachine":true,"region":"fra"}`, declared: true, on: true},
		{name: "boxd with its field off", driver: "boxd", config: `{"inMachine":false}`, declared: true},
		{name: "boxd with none", driver: "boxd", declared: true},
		{name: "boxd with a value that is not a boolean", driver: "boxd", config: `{"inMachine":"yes"}`, wantErr: "not a boolean"},
		{name: "archil, which declares none", driver: "archil", config: `{"region":"aws-us-east-1"}`},
		{name: "a driver that is not linked", driver: "nope", wantErr: "definition"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := Bootstrap{Driver: tc.driver}
			if tc.config != "" {
				b.DriverConfig = json.RawMessage(tc.config)
			}
			auth, on, err := inMachineAuth(b)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("inMachineAuth = %v, want an error naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || (auth != nil) != tc.declared || on != tc.on {
				t.Fatalf("inMachineAuth = %+v, %v, %v; want declared %v, on %v", auth, on, err, tc.declared, tc.on)
			}
		})
	}
}

// boxd's declaration is what the rule set is made from: its metadata
// adapter on the default gateway.
func TestBoxdDeclaresItsMetadataAdapter(t *testing.T) {
	auth, _, err := inMachineAuth(Bootstrap{Driver: "boxd"})
	if err != nil || auth == nil {
		t.Fatalf("boxd's in-machine auth = %+v, %v", auth, err)
	}
	if len(auth.Endpoints) != 1 || !auth.Endpoints[0].Gateway || auth.Endpoints[0].Port != 9002 {
		t.Fatalf("boxd's endpoints = %+v, want <gateway>:9002", auth.Endpoints)
	}
}

func TestDefaultGateway(t *testing.T) {
	gateway, err := defaultGateway(writeRouteTable(t, routeTable))
	if err != nil || gateway != netip.MustParseAddr("10.0.2.1") {
		t.Fatalf("defaultGateway = %v, %v; want 10.0.2.1", gateway, err)
	}
	noDefault := strings.Join(strings.Split(routeTable, "\n")[:2], "\n") + "\n"
	if _, err := defaultGateway(writeRouteTable(t, noDefault)); err == nil {
		t.Fatal("a route table with no default route has a gateway")
	}
	if _, err := defaultGateway(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing route table has a gateway")
	}
}

// Each endpoint is resolved as the machine sees it: the gateway from its
// route table, an address or CIDR as given, a name to every address it
// resolves to.
func TestInMachineTargets(t *testing.T) {
	routes := writeRouteTable(t, routeTable)
	resolver := &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.DNSError{Err: "no DNS in this test", IsNotFound: true}
	}}
	targets, err := inMachineTargets(context.Background(), []machine.MachineEndpoint{
		{Gateway: true, Port: 9002},
		{Host: "169.254.169.254", Port: 80},
		{Host: "fd00::/64", Port: 443},
		{Gateway: true, Port: 9002},
	}, routes, resolver)
	if err != nil {
		t.Fatal(err)
	}
	want := []inMachineTarget{
		{prefix: netip.MustParsePrefix("10.0.2.1/32"), port: 9002},
		{prefix: netip.MustParsePrefix("169.254.169.254/32"), port: 80},
		{prefix: netip.MustParsePrefix("fd00::/64"), port: 443},
	}
	if len(targets) != len(want) {
		t.Fatalf("targets = %+v, want %+v", targets, want)
	}
	for i := range want {
		if targets[i] != want[i] {
			t.Fatalf("targets = %+v, want %+v", targets, want)
		}
	}
	if _, err := inMachineTargets(context.Background(), []machine.MachineEndpoint{{Host: "metadata.invalid", Port: 80}}, routes, resolver); err == nil {
		t.Fatal("a name that does not resolve was fenced as nothing")
	}
	if _, err := inMachineTargets(context.Background(), []machine.MachineEndpoint{{Gateway: true, Port: 9002}}, filepath.Join(t.TempDir(), "missing"), resolver); err == nil {
		t.Fatal("a gateway that cannot be read was fenced as nothing")
	}
	if _, err := inMachineTargets(context.Background(), nil, routes, resolver); err == nil {
		t.Fatal("no endpoint is a fence of nothing")
	}
}

// The rule set resets every connection to a target but the engine's, and
// every one the machine routes, in a table of its own that it replaces.
func TestInMachineRules(t *testing.T) {
	rules := inMachineRules([]inMachineTarget{
		{prefix: netip.MustParsePrefix("10.0.2.1/32"), port: 9002},
		{prefix: netip.MustParsePrefix("fd00::/64"), port: 443},
	})
	for _, want := range []string{
		"table inet discobox_in_machine\ndelete table inet discobox_in_machine\ntable inet discobox_in_machine {",
		"type filter hook output priority filter; policy accept;",
		"meta mark != 0x64697363 ip daddr 10.0.2.1/32 tcp dport 9002 reject with tcp reset",
		"meta mark != 0x64697363 ip6 daddr fd00::/64 tcp dport 443 reject with tcp reset",
		"type filter hook forward priority filter; policy accept;\n\t\tip daddr 10.0.2.1/32 tcp dport 9002 reject with tcp reset\n\t\tip6 daddr fd00::/64 tcp dport 443 reject with tcp reset",
	} {
		if !strings.Contains(rules, want) {
			t.Fatalf("rules lack %q:\n%s", want, rules)
		}
	}
}
