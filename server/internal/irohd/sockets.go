package irohd

import (
	"fmt"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// socketsFileName is where the server remembers the UDP sockets its iroh
// endpoint bound, so the next start can ask for the same ports.
//
// It exists because the port is the one part of this server's address that a
// peer ID does not carry and discovery does not publish: a client learns it by
// connecting and keeps it (the CLI's peer-addrs memory). A server that binds a
// fresh port on every start throws all of that away at the moment every client
// is reconnecting, and sends them all back through the relay at once.
//
// It is a hint, not configuration. A port that has been taken in the meantime
// costs nothing but a fresh one, and a missing or unreadable file is a server
// binding the way it did before this existed.
const socketsFileName = "iroh_sockets"

// LoadSockets returns the sockets the server's iroh endpoint bound last time,
// for [endpoint.IrohConfig.PreferredBindAddrs].
//
// The file is all or nothing: one that half parses offers no ports rather than
// half of them. A family missing from a whole file is the endpoint's to put
// back (see [endpoint.IrohConfig.PreferredBindAddrs]), since only it knows what
// its ordinary bind would have asked for.
func LoadSockets(dataDir string) []netip.AddrPort {
	data, err := os.ReadFile(filepath.Join(dataDir, socketsFileName))
	if err != nil {
		return nil
	}
	var sockets []netip.AddrPort
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		socket, err := netip.ParseAddrPort(line)
		if err != nil || socket.Port() == 0 {
			return nil
		}
		sockets = append(sockets, socket)
	}
	return sockets
}

// RememberSockets records the sockets the server's iroh endpoint bound, for
// [endpoint.IrohConfig.Bound].
//
// A server that came back where it was writes nothing. One whose set changed
// is written, and one that lost a port it had is also logged, because that is
// the restart every client pays for: whatever they remembered is wrong now,
// and until each of them connects again through the relay, a relay having a
// bad day is an outage. A set that only gained a socket — a family the last
// start could not bind — kept every port a client could have learned, and is
// not news.
func RememberSockets(dataDir string, sockets []netip.AddrPort) error {
	if len(sockets) == 0 {
		return nil
	}
	previous := LoadSockets(dataDir)
	if slices.Equal(previous, sockets) {
		return nil
	}
	var lost []netip.AddrPort
	for _, socket := range previous {
		if !slices.Contains(sockets, socket) {
			lost = append(lost, socket)
		}
	}
	if len(lost) > 0 {
		log.Printf("iroh: bound %s; %s from the last start was unavailable, so peers that remembered it reach this server through its relay until they connect again",
			joinSockets(sockets), joinSockets(lost))
	}
	var contents strings.Builder
	for _, socket := range sockets {
		contents.WriteString(socket.String())
		contents.WriteByte('\n')
	}
	// Written beside and renamed into place, so a server killed mid-write
	// leaves the old file or the new one, never a torn one that LoadSockets
	// would discard.
	path := filepath.Join(dataDir, socketsFileName)
	tmp, err := os.CreateTemp(dataDir, socketsFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create iroh sockets temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(contents.String()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write iroh sockets: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close iroh sockets temp file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("install iroh sockets: %w", err)
	}
	return nil
}

func joinSockets(sockets []netip.AddrPort) string {
	out := make([]string, 0, len(sockets))
	for _, socket := range sockets {
		out = append(out, socket.String())
	}
	return strings.Join(out, " ")
}
