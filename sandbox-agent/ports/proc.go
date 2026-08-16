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

// listener is one socket in TCP_LISTEN as /proc/net/tcp and /proc/net/tcp6
// report it. Those tables are per network namespace, and sandbox-agent shares
// the sandbox's, so they list exactly the sockets a forward could reach.
type listener struct {
	Addr  netip.Addr
	Port  int
	Inode uint64
}

// tcpStateListen is TCP_LISTEN as the kernel prints it in the st column.
const tcpStateListen = "0A"

// procNetTCPFields is how many whitespace-separated columns a row must have to
// carry the ones this reads: local address (1), state (3), uid (7), inode (9).
const procNetTCPFields = 10

// scanListeners returns every listening TCP socket owned by uid. A table that
// does not exist is not an error — an IPv6-less kernel has no net/tcp6, and a
// platform with no procfs at all has neither, which reports no ports rather
// than failing the status it is part of.
func scanListeners(procRoot string, uid int64) ([]listener, error) {
	var (
		out  []listener
		errs []error
	)
	for _, name := range []string{"tcp", "tcp6"} {
		data, err := os.ReadFile(filepath.Join(procRoot, "net", name))
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		out = append(out, parseProcNetTCP(string(data), uid)...)
	}
	if len(out) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

func parseProcNetTCP(table string, uid int64) []listener {
	var out []listener
	for line := range strings.Lines(table) {
		fields := strings.Fields(line)
		if len(fields) < procNetTCPFields {
			continue
		}
		if !strings.EqualFold(fields[3], tcpStateListen) {
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
		out = append(out, listener{Addr: addr, Port: port, Inode: inode})
	}
	return out
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
