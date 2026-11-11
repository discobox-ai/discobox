package ports

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

// inetDiagMsg builds a struct inet_diag_msg the way the kernel lays it out:
// port and address in network order, uid and inode in host order.
func inetDiagMsg(family uint8, state uint8, addr netip.Addr, port uint16, uid, inode uint32) []byte {
	b := make([]byte, inetDiagMsgLen)
	b[0], b[1] = family, state
	binary.BigEndian.PutUint16(b[4:6], port)
	raw := addr.AsSlice()
	copy(b[8:8+len(raw)], raw)
	binary.NativeEndian.PutUint32(b[64:68], uid)
	binary.NativeEndian.PutUint32(b[68:72], inode)
	return b
}

func TestParseInetDiagMsgReadsEachFieldInItsOwnByteOrder(t *testing.T) {
	cases := []struct {
		name string
		msg  []byte
		want listener
		ok   bool
	}{
		{
			name: "ipv4",
			msg:  inetDiagMsg(afInet, tcpListen, netip.MustParseAddr("127.0.0.1"), 5173, 1000, 44001),
			want: listener{Network: networkTCP, Addr: netip.MustParseAddr("127.0.0.1"), Port: 5173, Socket: 44001},
			ok:   true,
		},
		{
			name: "ipv6",
			msg:  inetDiagMsg(afInet6, tcpListen, netip.MustParseAddr("fe80::1"), 8443, 1000, 70000),
			want: listener{Network: networkTCP, Addr: netip.MustParseAddr("fe80::1"), Port: 8443, Socket: 70000},
			ok:   true,
		},
		{
			name: "v4-mapped is unmapped",
			msg:  inetDiagMsg(afInet6, tcpListen, netip.MustParseAddr("::ffff:10.0.0.2"), 3000, 1000, 9),
			want: listener{Network: networkTCP, Addr: netip.MustParseAddr("10.0.0.2"), Port: 3000, Socket: 9},
			ok:   true,
		},
		{name: "another uid", msg: inetDiagMsg(afInet, tcpListen, netip.MustParseAddr("0.0.0.0"), 80, 0, 1)},
		{name: "not listening", msg: inetDiagMsg(afInet, 1, netip.MustParseAddr("0.0.0.0"), 80, 1000, 1)},
		{name: "port zero", msg: inetDiagMsg(afInet, tcpListen, netip.MustParseAddr("0.0.0.0"), 0, 1000, 1)},
		{name: "short", msg: inetDiagMsg(afInet, tcpListen, netip.MustParseAddr("0.0.0.0"), 80, 1000, 1)[:inetDiagMsgLen-1]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseInetDiagMsg(tc.msg, 1000)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("parse = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// sockDiagFixture is a procfs root whose TCP table lists one listener and
// whose UDP table lists another, so a scan's answer says which source each
// port came from.
func sockDiagFixture(t *testing.T) Procfs {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	tables := map[string]string{
		// 127.0.0.1:4000, listening.
		"tcp": procNetTCPHeader + "   0: 0100007F:0FA0 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 1 1 0000 100 0 0 10 0\n",
		// 0.0.0.0:53, unconnected.
		"udp": procNetUDPHeader + "  1: 00000000:0035 00000000:0000 07 00000000:00000000 00:00000000 00000000  1000        0 2 2 0000000000000000 0\n",
	}
	for name, table := range tables {
		if err := os.WriteFile(filepath.Join(root, "net", name), []byte(table), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Procfs{Root: root}
}

func portsOf(ls []listener) []string {
	var out []string
	for _, l := range ls {
		out = append(out, fmt.Sprintf("%s/%d", l.Network, l.Port))
	}
	return out
}

// The TCP listeners come from sock_diag and the UDP ones from procfs: a
// procfs TCP table read beside a dump would cost exactly what the dump saves.
func TestSockDiagReadsTCPFromTheDumpAndUDPFromProcfs(t *testing.T) {
	s := &sockDiag{Procfs: sockDiagFixture(t), dump: func(family uint8) ([][]byte, error) {
		if family != afInet {
			return nil, nil
		}
		return [][]byte{inetDiagMsg(afInet, tcpListen, netip.MustParseAddr("127.0.0.1"), 5173, 1000, 3)}, nil
	}}
	got, _, err := s.scan(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(portsOf(got)) != "[tcp/5173 udp/53]" {
		t.Fatalf("scan = %v, want tcp/5173 from the dump and udp/53 from procfs", portsOf(got))
	}
}

// A kernel that refuses the dump has no sock_diag, and will not grow one:
// that scan and every one after read procfs, without asking again.
func TestSockDiagFallsBackToProcfsForGoodWhenTheKernelRefuses(t *testing.T) {
	calls := 0
	s := &sockDiag{Procfs: sockDiagFixture(t), dump: func(uint8) ([][]byte, error) {
		calls++
		return nil, fmt.Errorf("sock_diag dump: %w", errSockDiagUnsupported)
	}}
	for range 3 {
		got, _, err := s.scan(context.Background(), 1000)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(portsOf(got)) != "[tcp/4000 udp/53]" {
			t.Fatalf("scan = %v, want procfs's tcp/4000 and udp/53", portsOf(got))
		}
	}
	if calls != 1 {
		t.Fatalf("dump asked %d times, want once: a refusal is for good", calls)
	}
}

// Any other failure is that scan's: it reads procfs, and the next scan asks
// sock_diag again.
func TestSockDiagRetriesAfterAPassingFailure(t *testing.T) {
	fail := true
	s := &sockDiag{Procfs: sockDiagFixture(t), dump: func(family uint8) ([][]byte, error) {
		if fail {
			return nil, errors.New("sock_diag receive: resource temporarily unavailable")
		}
		if family != afInet {
			return nil, nil
		}
		return [][]byte{inetDiagMsg(afInet, tcpListen, netip.MustParseAddr("127.0.0.1"), 5173, 1000, 3)}, nil
	}}
	got, _, err := s.scan(context.Background(), 1000)
	if err != nil || fmt.Sprint(portsOf(got)) != "[tcp/4000 udp/53]" {
		t.Fatalf("scan = %v, %v; want procfs's answer after a failed dump", portsOf(got), err)
	}
	fail = false
	got, _, err = s.scan(context.Background(), 1000)
	if err != nil || fmt.Sprint(portsOf(got)) != "[tcp/5173 udp/53]" {
		t.Fatalf("scan = %v, %v; want the dump asked again", portsOf(got), err)
	}
}
