package ports

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net/netip"
	"sync/atomic"
)

// sockDiag is Linux's Scanner: the kernel's sock_diag netlink interface for
// listening TCP sockets, and procfs for UDP ones and wherever the kernel has
// no sock_diag (ADR 26-11-03-815 §1).
//
// It exists for cost, not for anything procfs gets wrong. Reading
// /proc/net/tcp walks the kernel's whole established-socket hash, which is
// sized by the host's memory rather than by the sockets in the box: a few
// milliseconds per read on a large host, twice a second. A sock_diag dump
// asked for TCP_LISTEN only walks the listening hash, a few hundred
// microseconds. UDP has no such split, and its tables are cheap to read, so
// they stay on procfs.
//
// sock_diag needs CONFIG_INET_DIAG and its TCP module. A kernel without them —
// a VM box's minimal kernel, a host that cannot load modules, a gVisor
// sandbox — refuses the dump, and the scanner then reads procfs for the rest of
// its life: a refusal is the kernel's answer, not a passing failure. Any other
// error is: that scan reads procfs, and the next tries sock_diag again.
type sockDiag struct {
	Procfs Procfs
	// dump answers one family's TCP_LISTEN dump as the inet_diag_msg payloads
	// it carried: the platform's netlink round trip, which tests replace.
	dump func(family uint8) ([][]byte, error)
	// refused is set once the kernel has refused a dump.
	refused atomic.Bool
}

// errSockDiagUnsupported is what dump returns when the kernel does not do
// sock_diag for TCP at all.
var errSockDiagUnsupported = errors.New("the kernel has no sock_diag for TCP")

const (
	afInet  = 2
	afInet6 = 10
	// tcpListen is TCP_LISTEN, the state whose bit the dump asks for.
	tcpListen = 10
)

func (s *sockDiag) scan(ctx context.Context, uid int64) ([]listener, error) {
	if s.refused.Load() {
		return s.Procfs.scan(ctx, uid)
	}
	tcp, err := s.listeners(uid)
	if err != nil {
		if errors.Is(err, errSockDiagUnsupported) && !s.refused.Swap(true) {
			slog.Info("ports: the kernel has no sock_diag; listening sockets are read from procfs", "error", err)
		}
		return s.Procfs.scan(ctx, uid)
	}
	udp, err := s.Procfs.scanTables(procNetUDPTables, uid)
	if err != nil && len(tcp) == 0 {
		return nil, err
	}
	return append(tcp, udp...), nil
}

// listeners is every listening TCP socket uid holds, IPv4 and IPv6, as
// sock_diag lists them.
func (s *sockDiag) listeners(uid int64) ([]listener, error) {
	var out []listener
	for _, family := range []uint8{afInet, afInet6} {
		msgs, err := s.dump(family)
		if err != nil {
			return nil, err
		}
		for _, msg := range msgs {
			if l, ok := parseInetDiagMsg(msg, uid); ok {
				out = append(out, l)
			}
		}
	}
	return out, nil
}

// inetDiagMsgLen is sizeof(struct inet_diag_msg): family, state, timer and
// retrans bytes, the 48-byte inet_diag_sockid, then expires, rqueue, wqueue,
// uid and inode, each a u32.
const inetDiagMsgLen = 72

// parseInetDiagMsg reads one struct inet_diag_msg (include/uapi/linux/
// inet_diag.h). Its byte orders are mixed, and that is the trap: the port and
// address are network order, as the sockid's __be16/__be32 say, while uid and
// inode are host order. A socket that is not listening or not uid's is
// dropped, as procfs drops it.
func parseInetDiagMsg(b []byte, uid int64) (listener, bool) {
	if len(b) < inetDiagMsgLen {
		return listener{}, false
	}
	family, state := b[0], b[1]
	if state != tcpListen {
		return listener{}, false
	}
	if int64(binary.NativeEndian.Uint32(b[64:68])) != uid {
		return listener{}, false
	}
	port := int(binary.BigEndian.Uint16(b[4:6]))
	if port == 0 {
		return listener{}, false
	}
	var addr netip.Addr
	switch family {
	case afInet:
		addr = netip.AddrFrom4([4]byte(b[8:12]))
	case afInet6:
		addr = netip.AddrFrom16([16]byte(b[8:24]))
	default:
		return listener{}, false
	}
	return listener{
		Network: networkTCP,
		// Unmapped as procfs's is: an IPv4 address bound through an AF_INET6
		// socket is still that IPv4 address.
		Addr:   addr.Unmap(),
		Port:   port,
		Socket: uint64(binary.NativeEndian.Uint32(b[68:72])),
	}, true
}
