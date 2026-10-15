//go:build linux

package dnsstub

import (
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// inNetnsEnv marks the re-executed test binary that runs inside its own network
// namespace, where it may change addresses and rules freely.
const inNetnsEnv = "DNSSTUB_TEST_IN_NETNS"

// TestClaimAddressKeepsTheStubOffThePoolsNetworkOnly claims the stub's address
// in a network namespace of its own, twice, as a restarted stub would: the
// address is on lo, what arrives on the pool's network is dropped by one rule,
// and nothing drops the nested Docker's bridges.
func TestClaimAddressKeepsTheStubOffThePoolsNetworkOnly(t *testing.T) {
	ctx := t.Context()
	if os.Getenv(inNetnsEnv) == "" {
		if os.Geteuid() != 0 {
			t.Skip("needs root, to give the test a network namespace of its own")
		}
		if _, err := exec.LookPath("iptables"); err != nil {
			t.Skip("needs iptables")
		}
		//nolint:gosec // G204: re-executes this test binary, running this one test.
		cmd := exec.CommandContext(ctx, "unshare", "--net", os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
		cmd.Env = append(os.Environ(), inNetnsEnv+"=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("in a network namespace of its own: %v\n%s", err, out)
		}
		t.Logf("%s", out)
		return
	}

	addr := netip.MustParseAddr("169.254.53.53")
	prefix := "169.254.53.53/32"
	for range 2 {
		if err := ClaimAddress(ctx, addr); err != nil {
			t.Fatal(err)
		}
	}

	addrs, err := exec.CommandContext(ctx, "ip", "-o", "address", "show", "dev", "lo").CombinedOutput()
	if err != nil {
		t.Fatalf("ip address: %v: %s", err, addrs)
	}
	if !strings.Contains(string(addrs), prefix) {
		t.Errorf("lo does not hold %s:\n%s", prefix, addrs)
	}
	rules, err := exec.CommandContext(ctx, "iptables", "-w", "-S", "INPUT").CombinedOutput()
	if err != nil {
		t.Fatalf("iptables -S: %v: %s", err, rules)
	}
	var drops []string
	for line := range strings.Lines(string(rules)) {
		if strings.Contains(line, prefix) {
			drops = append(drops, strings.TrimSpace(line))
		}
	}
	want := "-A INPUT -d " + prefix + " -i " + pooledInterfaces + " -j DROP"
	if len(drops) != 1 || drops[0] != want {
		t.Errorf("rules for %s = %q, want exactly %q", prefix, drops, want)
	}
}
