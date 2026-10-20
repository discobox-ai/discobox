//go:build linux

package dnsstub

import (
	"bytes"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// inNetnsEnv marks the re-executed test binary that runs inside its own network
// namespace, where it may change addresses and rules freely.
const inNetnsEnv = "DNSSTUB_TEST_IN_NETNS"

// TestClaimAddressKeepsTheStubOffThePoolsNetworkOnly claims the stub's
// address in a network namespace of its own, twice, as a restarted stub
// would, then has a neighbor send it a query over each kind of interface a
// box has: the address is on lo, one rule per pooled name drops it on the
// box's own network — a container's eth0 or a machine's ens3 — and nothing
// drops the nested Docker's bridges, whatever they are called.
func TestClaimAddressKeepsTheStubOffThePoolsNetworkOnly(t *testing.T) {
	ctx := t.Context()
	if os.Getenv(inNetnsEnv) == "" {
		if os.Geteuid() != 0 {
			t.Skip("needs root, to give the test a network namespace of its own")
		}
		for _, tool := range []string{"iptables", "unshare", "nsenter", "bash"} {
			if _, err := exec.LookPath(tool); err != nil {
				t.Skipf("needs %s", tool)
			}
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
	mustRun(t, "ip", "link", "set", "lo", "up")
	for range 2 {
		if err := ClaimAddress(ctx, addr); err != nil {
			t.Fatal(err)
		}
	}

	addrs := mustRun(t, "ip", "-o", "address", "show", "dev", "lo")
	if !strings.Contains(addrs, prefix) {
		t.Errorf("lo does not hold %s:\n%s", prefix, addrs)
	}
	var drops []string
	for line := range strings.Lines(mustRun(t, "iptables", "-w", "-S", "INPUT")) {
		if strings.Contains(line, prefix) {
			drops = append(drops, strings.TrimSpace(line))
		}
	}
	var want []string
	for _, iface := range pooledInterfaces {
		want = append([]string{"-A INPUT -d " + prefix + " -i " + iface + " -j DROP"}, want...)
	}
	if !slices.Equal(drops, want) {
		t.Errorf("rules for %s = %q, want exactly %q", prefix, drops, want)
	}

	listener, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(addr, 53)))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	answered := func(payload string) bool {
		t.Helper()
		buf := make([]byte, 512)
		for {
			if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			n, _, err := listener.ReadFromUDP(buf)
			if err != nil {
				return false
			}
			if string(buf[:n]) == payload {
				return true
			}
		}
	}

	// The box itself, as Docker's embedded resolver forwards from it.
	conn, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(addr, 53)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("lo")); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if !answered("lo") {
		t.Error("a query from the box itself was not answered")
	}

	for i, tc := range []struct {
		iface    string
		answered bool
	}{
		{"eth0", false},           // a container's pool network
		{"ens3", false},           // a provider machine's network
		{"enp0s1", false},         // and another provider's naming
		{"docker0", true},         // the nested Docker's default bridge
		{"br-0123456789ab", true}, // a network the nested Docker created
		{"kindnet", true},         // one given its own bridge name
		{"tun0", true},            // a VPN the sandbox's root started
	} {
		peer := neighbor(t, tc.iface, i)
		payload := "via " + tc.iface
		//nolint:gosec // G204: nsenter into the test's own neighbor, sending a fixed payload.
		send := exec.CommandContext(ctx, "nsenter", "-t", peer, "-n", "bash", "-c",
			fmt.Sprintf("printf %%s %q > /dev/udp/%s/53", payload, addr))
		if out, err := send.CombinedOutput(); err != nil {
			t.Fatalf("send %s: %v: %s", payload, err, out)
		}
		if got := answered(payload); got != tc.answered {
			t.Errorf("a query arriving on %s answered = %v, want %v", tc.iface, got, tc.answered)
		}
	}
}

// neighbor is a network namespace of its own joined to this one by a veth
// whose end here is named iface, routing the stub's address through it. It
// returns a PID inside it for nsenter.
func neighbor(t *testing.T, iface string, i int) string {
	t.Helper()
	sleep := exec.CommandContext(t.Context(), "unshare", "--net", "sleep", "60")
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sleep.Process.Kill(); _ = sleep.Wait() })
	pid := fmt.Sprint(sleep.Process.Pid)
	// unshare has made the namespace once its child is sleep.
	for deadline := time.Now().Add(5 * time.Second); ; {
		comm, _ := os.ReadFile("/proc/" + pid + "/comm")
		if bytes.Equal(bytes.TrimSpace(comm), []byte("sleep")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the neighbor's namespace never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	here, there := fmt.Sprintf("10.99.%d.1", i), fmt.Sprintf("10.99.%d.2", i)
	mustRun(t, "ip", "link", "add", iface, "type", "veth", "peer", "name", "peer", "netns", pid)
	mustRun(t, "ip", "address", "add", here+"/24", "dev", iface)
	mustRun(t, "ip", "link", "set", iface, "up")
	for _, args := range [][]string{
		{"ip", "link", "set", "lo", "up"},
		{"ip", "address", "add", there + "/24", "dev", "peer"},
		{"ip", "link", "set", "peer", "up"},
		{"ip", "route", "add", "169.254.53.53/32", "via", here},
	} {
		mustRun(t, "nsenter", append([]string{"-t", pid, "-n"}, args...)...)
	}
	return pid
}

func mustRun(t *testing.T, name string, args ...string) string {
	t.Helper()
	//nolint:gosec // G204: the test's own fixed commands.
	out, err := exec.CommandContext(t.Context(), name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}
