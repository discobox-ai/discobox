package ports

import (
	"context"
	"net/netip"
	"strconv"
	"strings"
)

// lsofScanner is darwin's Scanner. darwin has no procfs; its socket tables are
// the kernel's pcblist sysctls, whose records are private, versioned structs
// that only the system's own tools are built against. lsof is one of those
// tools and has a field output made for programs to read, so the scan runs it
// and reads that.
//
// Only the running of lsof and the ephemeral range are darwin's own; what lsof
// prints is parsed here, on every platform, so its tests run everywhere.
type lsofScanner struct {
	// run runs lsof with these arguments and returns its stdout.
	run func(ctx context.Context, args []string) ([]byte, error)
	// ephemeral is the range the kernel autobinds a client's socket from.
	ephemeral func() portRange
}

// darwinEphemeralRange is darwin's default autobind range, used when the
// sysctls cannot be read.
var darwinEphemeralRange = portRange{low: 49152, high: 65535}

// lsofArgs selects every listening TCP socket and every UDP socket held by a
// process uid runs, in field output:
//
//   - -n and -P print addresses and ports as numbers, never names.
//   - -a ANDs the -u selection with the -i ones; the two -i are ORed, and
//     -sTCP:LISTEN narrows only the TCP one.
//   - -F asks for the process (p), descriptor (f), address family (t), socket
//     address (d), protocol (P), name (n), and TCP state (T) fields.
//
// A process run by uid is as close as darwin comes to Linux's socket owner:
// the socket belongs to whoever created it, which is the process holding it.
func lsofArgs(uid int64) []string {
	return []string{
		"-n", "-P", "-w",
		"-a", "-u", strconv.FormatInt(uid, 10),
		"-iTCP", "-sTCP:LISTEN", "-iUDP",
		"-F", "pftdPnT",
	}
}

func (s lsofScanner) scan(ctx context.Context, uid int64) ([]listener, error) {
	out, err := s.run(ctx, lsofArgs(uid))
	if err != nil {
		return nil, err
	}
	ephemeral := s.ephemeral()
	var listeners []listener
	for _, entry := range parseLsof(string(out)) {
		if entry.Network == networkUDP && ephemeral.contains(entry.Port) {
			continue
		}
		listeners = append(listeners, entry)
	}
	return listeners, nil
}

// lsofFile is one file set of lsof's field output: the fields between one f
// line and the next.
type lsofFile struct {
	pid, fd  string
	family   string
	device   string
	protocol string
	name     string
	state    string
}

// parseLsof reads lsof's field output into listeners. Each line is one field,
// its first byte saying which; a p line starts a process and an f line one of
// its files. A socket two processes share — a server that forked its workers
// after binding — is listed under each, and reported once.
func parseLsof(out string) []listener {
	var (
		listeners []listener
		seen      = map[endpointSocket]bool{}
		pid       string
		file      *lsofFile
	)
	flush := func() {
		if file == nil {
			return
		}
		entry, ok := file.listener()
		file = nil
		if !ok {
			return
		}
		key := endpointSocket{network: entry.Network, socket: entry.Socket}
		if seen[key] {
			return
		}
		seen[key] = true
		listeners = append(listeners, entry)
	}
	for line := range strings.Lines(out) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		value := line[1:]
		switch line[0] {
		case 'p':
			flush()
			pid = value
		case 'f':
			flush()
			file = &lsofFile{pid: pid, fd: value}
		case 't':
			if file != nil {
				file.family = value
			}
		case 'd':
			if file != nil {
				file.device = value
			}
		case 'P':
			if file != nil {
				file.protocol = value
			}
		case 'n':
			if file != nil {
				file.name = value
			}
		case 'T':
			if state, ok := strings.CutPrefix(value, "ST="); ok && file != nil {
				file.state = state
			}
		}
	}
	flush()
	return listeners
}

// endpointSocket keys a socket for de-duplication across the processes that
// share it.
type endpointSocket struct {
	network network
	socket  uint64
}

// listener reads one file set as a socket a server could be behind: a TCP
// socket lsof says is listening, or a UDP socket with no peer.
func (f *lsofFile) listener() (listener, bool) {
	var net network
	switch f.protocol {
	case "TCP":
		// -sTCP:LISTEN already selected only listening sockets; the state is
		// checked anyway, so a socket lsof reports in some other state is
		// never read as a server.
		if f.state != "" && f.state != "LISTEN" {
			return listener{}, false
		}
		net = networkTCP
	case "UDP":
		net = networkUDP
	default:
		return listener{}, false
	}
	// A peer is printed after "->": a connected UDP socket is a client's, and
	// a listening TCP socket never has one.
	if strings.Contains(f.name, "->") {
		return listener{}, false
	}
	addr, port, ok := parseLsofName(f.name, f.family)
	if !ok {
		return listener{}, false
	}
	return listener{Network: net, Addr: addr, Port: port, Socket: f.socket()}, true
}

// socket is the socket's identity. lsof prints a socket's kernel address as
// its device, which is what changes when a server replaces its socket; with
// none printed, the descriptor in its process stands in, which changes when
// the server restarts.
func (f *lsofFile) socket() uint64 {
	if hexAddr, ok := strings.CutPrefix(f.device, "0x"); ok {
		if value, err := strconv.ParseUint(hexAddr, 16, 64); err == nil {
			return value
		}
	}
	pid, _ := strconv.ParseUint(f.pid, 10, 32)
	fd, _ := strconv.ParseUint(f.fd, 10, 32)
	return pid<<32 | fd
}

// parseLsofName reads a local address as lsof prints it: "*:8080" for a
// wildcard bind, "127.0.0.1:8080", or "[::1]:8080" for IPv6. A wildcard is
// spelled the same for both families, so family — lsof's t field, "IPv4" or
// "IPv6" — says which wildcard it is. A port of "*" is a socket bound to
// nothing.
func parseLsofName(name, family string) (netip.Addr, int, bool) {
	sep := strings.LastIndexByte(name, ':')
	if sep < 0 {
		return netip.Addr{}, 0, false
	}
	host, portText := name[:sep], name[sep+1:]
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return netip.Addr{}, 0, false
	}
	var addr netip.Addr
	switch {
	case host == "*" && family == "IPv6":
		addr = netip.IPv6Unspecified()
	case host == "*":
		addr = netip.IPv4Unspecified()
	default:
		addr, err = netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
		if err != nil {
			return netip.Addr{}, 0, false
		}
	}
	// The same unmapping procfs's reader does, for the same reason: one port
	// is not reported under two spellings of one address.
	return addr.Unmap(), int(port), true
}
