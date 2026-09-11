package ports

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// listener is one socket a server could be behind, as /proc/net/{tcp,udp}{,6}
// report it: a TCP socket in TCP_LISTEN, or a UDP socket nothing has connected.
// Those tables are per network namespace, and sandbox-agent shares the
// sandbox's, so they list exactly the sockets a forward could reach.
type listener struct {
	Network network
	Addr    netip.Addr
	Port    int
	Inode   uint64
}

// network is which table a socket came from. A TCP port and a UDP port with the
// same number are two ports (ADR 0109 §1), so it is half of every key.
type network string

const (
	networkTCP network = "tcp"
	networkUDP network = "udp"
)

// tcpStateListen is TCP_LISTEN as the kernel prints it in the st column.
const tcpStateListen = "0A"

// udpStateUnconnected is the st column of a UDP socket that has not called
// connect(2). The kernel reuses TCP's state numbers for UDP, and this one is
// TCP_CLOSE; a connected socket is 01, TCP_ESTABLISHED. A server receives from
// anybody and so never connects, which makes this the closest UDP has to a
// listen state — and a client using sendto never connects either, which is why
// it is not enough on its own (see ephemeralRange).
const udpStateUnconnected = "07"

// procNetFields is how many whitespace-separated columns a row must have to
// carry the ones this reads: local address (1), state (3), uid (7), inode (9).
// The TCP and UDP tables share that layout.
const procNetFields = 10

// procNetTable is one of the four tables a scan reads.
type procNetTable struct {
	name    string
	network network
	state   string
}

var procNetTables = []procNetTable{
	{name: "tcp", network: networkTCP, state: tcpStateListen},
	{name: "tcp6", network: networkTCP, state: tcpStateListen},
	{name: "udp", network: networkUDP, state: udpStateUnconnected},
	{name: "udp6", network: networkUDP, state: udpStateUnconnected},
}

// scanListeners returns every listening TCP socket and every bound,
// unconnected UDP socket owned by uid. A table that does not exist is not an
// error — an IPv6-less kernel has no net/tcp6, and a platform with no procfs at
// all has none of them, which reports no ports rather than failing the status
// it is part of.
//
// A UDP socket on a port inside the ephemeral range is dropped: that is where
// the kernel puts a client's socket when it binds nothing, and an unconnected
// client looks exactly like a server in every other column (ADR 0109 §1).
func scanListeners(procRoot string, uid int64) ([]listener, error) {
	var (
		out  []listener
		errs []error
	)
	ephemeral := readEphemeralRange(procRoot)
	for _, table := range procNetTables {
		data, err := os.ReadFile(filepath.Join(procRoot, "net", table.name))
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		for _, entry := range parseProcNet(string(data), table, uid) {
			if entry.Network == networkUDP && ephemeral.contains(entry.Port) {
				continue
			}
			out = append(out, entry)
		}
	}
	if len(out) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

func parseProcNet(table string, from procNetTable, uid int64) []listener {
	var out []listener
	for line := range strings.Lines(table) {
		fields := strings.Fields(line)
		if len(fields) < procNetFields {
			continue
		}
		if !strings.EqualFold(fields[3], from.state) {
			continue
		}
		owner, err := strconv.ParseInt(fields[7], 10, 64)
		if err != nil || owner != uid {
			continue
		}
		addr, port, ok := parseHexAddrPort(fields[1])
		if !ok {
			continue
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, listener{Network: from.network, Addr: addr, Port: port, Inode: inode})
	}
	return out
}

// portRange is an inclusive range of port numbers.
type portRange struct{ low, high int }

func (r portRange) contains(port int) bool { return port >= r.low && port <= r.high }

// defaultEphemeralRange is the kernel's own default for ip_local_port_range,
// used when the sysctl cannot be read.
var defaultEphemeralRange = portRange{low: 32768, high: 60999}

// readEphemeralRange is the range the kernel autobinds from in this network
// namespace — the sysctl is per namespace, and procfs shows the reader's. It
// is read on every scan rather than once, because it is cheap and because a
// sandbox user with root may change it.
//
// The IPv4 knob governs IPv6 too; there is no ipv6 twin.
func readEphemeralRange(procRoot string) portRange {
	data, err := os.ReadFile(filepath.Join(procRoot, "sys", "net", "ipv4", "ip_local_port_range"))
	if err != nil {
		return defaultEphemeralRange
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return defaultEphemeralRange
	}
	low, errLow := strconv.Atoi(fields[0])
	high, errHigh := strconv.Atoi(fields[1])
	if errLow != nil || errHigh != nil || low < 1 || high > 65535 || low > high {
		return defaultEphemeralRange
	}
	return portRange{low: low, high: high}
}

// parseHexAddrPort decodes a local_address column ("0100007F:1F90"). The
// kernel prints the address as %08X per 32-bit word, so each word is the
// numeric value of four network-order address bytes read in *host* order — on
// a little-endian machine 127.0.0.1 prints as 0100007F. Writing the parsed
// value back out in native order therefore recovers the address bytes on
// either endianness, where a blind byte reversal would only be right on one.
func parseHexAddrPort(field string) (netip.Addr, int, bool) {
	host, portHex, ok := strings.Cut(field, ":")
	if !ok {
		return netip.Addr{}, 0, false
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil || port == 0 {
		return netip.Addr{}, 0, false
	}
	raw, err := hex.DecodeString(host)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.Addr{}, 0, false
	}
	for i := 0; i < len(raw); i += 4 {
		binary.NativeEndian.PutUint32(raw[i:i+4], binary.BigEndian.Uint32(raw[i:i+4]))
	}
	addr, ok := netip.AddrFromSlice(raw)
	if !ok {
		return netip.Addr{}, 0, false
	}
	// An IPv4 socket appears in net/tcp6 as a v4-mapped address when something
	// bound it through an AF_INET6 socket; unmapping keeps one port from being
	// reported under two spellings of the same address.
	return addr.Unmap(), int(port), true
}
