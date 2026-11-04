package ports

import (
	"errors"
	"maps"
	"net"
	"os"
	"slices"
	"testing"
	"time"
)

// sockDiag and procfs read the same kernel, so on a kernel that has both they
// must list the same listeners — the same address, port and inode for each.
// This is what catches a field read from the wrong offset or in the wrong
// byte order, which the struct layout gives every chance of.
func TestSockDiagListsWhatProcfsLists(t *testing.T) {
	if _, err := sockDiagDump(afInet); errors.Is(err, errSockDiagUnsupported) {
		t.Skipf("this kernel has no sock_diag for TCP: %v", err)
	}
	var mine []net.Listener
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0", "[::]:0"} {
		network := "tcp4"
		if addr[0] == '[' {
			network = "tcp6"
		}
		l, err := (&net.ListenConfig{}).Listen(t.Context(), network, addr)
		if err != nil {
			t.Logf("listen %s: %v", addr, err)
			continue
		}
		mine = append(mine, l)
		t.Cleanup(func() { l.Close() })
	}
	if len(mine) == 0 {
		t.Fatal("could not listen on any loopback address")
	}

	uid := int64(os.Getuid())
	diag := &sockDiag{Procfs: Procfs{Root: "/proc"}, dump: sockDiagDump}
	byKey := func(ls []listener) map[listener]bool {
		out := map[listener]bool{}
		for _, l := range ls {
			if l.Network == networkTCP {
				out[l] = true
			}
		}
		return out
	}
	// Other tests of this uid listen and close as they run; read the two
	// scans until they agree rather than failing on a socket that came or
	// went between them.
	var fromDiag, fromProc map[listener]bool
	for range 20 {
		d, err := diag.listeners(uid)
		if err != nil {
			t.Fatal(err)
		}
		p, err := Procfs{Root: "/proc"}.scanTables(procNetTCPTables, uid)
		if err != nil {
			t.Fatal(err)
		}
		fromDiag, fromProc = byKey(d), byKey(p)
		if maps.Equal(fromDiag, fromProc) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !maps.Equal(fromDiag, fromProc) {
		t.Fatalf("sock_diag lists %v\nprocfs lists %v", slices.Collect(maps.Keys(fromDiag)), slices.Collect(maps.Keys(fromProc)))
	}
	for _, l := range mine {
		port := l.Addr().(*net.TCPAddr).Port
		found := false
		for k := range fromDiag {
			found = found || k.Port == port
		}
		if !found {
			t.Fatalf("sock_diag does not list this test's own listener %s", l.Addr())
		}
	}
}
