package portforward

import (
	"context"
	"net"
	"strconv"
)

// DefaultSearch is how far above a remote port the search for a free local
// port goes before giving up on staying close to it.
const DefaultSearch = 64

// privilegedOffset is where the search restarts for a port this process
// probably cannot bind. It is 8000 because that is where the unprivileged
// twins of the well-known ports already live: 80 becomes 8080 and 443 becomes
// 8443, which is what a person reading the line expects to see.
const privilegedOffset = 8000

// listenNearest binds the port closest to want that is free, so a sandbox's
// 8080 stays 8080 locally when nothing else has it and becomes 8081 when
// something does.
func listenNearest(ctx context.Context, address string, want, search int) (net.Listener, error) {
	var config net.ListenConfig
	return bindNearest(want, search, func(port int) (net.Listener, error) {
		return config.Listen(ctx, "tcp", net.JoinHostPort(address, strconv.Itoa(port)))
	})
}

// listenPacketNearest is listenNearest for a UDP port. The two port spaces are
// separate, so a UDP binding's search is unaffected by what TCP holds, and the
// other way round.
func listenPacketNearest(ctx context.Context, address string, want, search int) ([]net.PacketConn, error) {
	addresses := packetAddresses(ctx, address)
	return bindNearest(want, search, func(port int) ([]net.PacketConn, error) {
		return listenPackets(ctx, addresses, port)
	})
}

// packetAddresses is where a UDP binding on address listens: address itself,
// and for a loopback address the other family's loopback too, when this
// machine has one.
//
// A UDP client given "localhost" sends to whichever address resolves first —
// ::1 on macOS and on many Linux systems — and nothing tells it to try the
// other, since no datagram is refused in a way a UDP client hears. A binding
// on 127.0.0.1 alone is then silent to it. A TCP client falls back by itself,
// which is why only UDP bindings take both.
func packetAddresses(ctx context.Context, address string) []string {
	ip := net.ParseIP(address)
	if ip == nil || !ip.IsLoopback() {
		return []string{address}
	}
	other := "::1"
	if ip.To4() == nil {
		other = "127.0.0.1"
	}
	probe, err := new(net.ListenConfig).ListenPacket(ctx, "udp", net.JoinHostPort(other, "0"))
	if err != nil {
		return []string{address}
	}
	_ = probe.Close()
	return []string{address, other}
}

// listenPackets binds port on every address, or on none: a binding that
// answered on one loopback but not the other would be the silence
// packetAddresses exists to prevent. Port 0 takes whatever the first address is
// given, and the rest follow it.
func listenPackets(ctx context.Context, addresses []string, port int) ([]net.PacketConn, error) {
	var config net.ListenConfig
	sockets := make([]net.PacketConn, 0, len(addresses))
	for _, address := range addresses {
		socket, err := config.ListenPacket(ctx, "udp", net.JoinHostPort(address, strconv.Itoa(port)))
		if err != nil {
			for _, bound := range sockets {
				_ = bound.Close()
			}
			return nil, err
		}
		sockets = append(sockets, socket)
		port = addrPort(socket.LocalAddr())
	}
	return sockets, nil
}

// bindNearest runs bind over the search, nearest port first, and returns the
// first thing it bound.
//
// Every bind error is treated as "try the next one" rather than matched
// against EADDRINUSE: the outcomes worth telling apart here are "got a port"
// and "got none", and matching errno per platform buys nothing. If the whole
// search fails, any port at all beats no forward — the caller prints the
// number it got — and only if that fails too does the first error surface.
func bindNearest[T any](want, search int, bind func(port int) (T, error)) (T, error) {
	var firstErr error
	try := func(port int) (T, bool) {
		bound, err := bind(port)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return bound, false
		}
		return bound, true
	}
	for _, span := range searchSpans(want, search) {
		for port := span.start; port < span.start+span.count && port <= 65535; port++ {
			if bound, ok := try(port); ok {
				return bound, nil
			}
		}
	}
	if bound, ok := try(0); ok {
		return bound, nil
	}
	var none T
	return none, firstErr
}

// listenExact binds want and nothing else, for a forward whose local number is
// not the caller's to choose — see Options.Exact.
func listenExact(ctx context.Context, address string, want int) (net.Listener, error) {
	var config net.ListenConfig
	return config.Listen(ctx, "tcp", net.JoinHostPort(address, strconv.Itoa(want)))
}

// listenPacketExact is listenExact for a UDP port.
func listenPacketExact(ctx context.Context, address string, want int) ([]net.PacketConn, error) {
	return listenPackets(ctx, packetAddresses(ctx, address), want)
}

type searchSpan struct {
	start int
	count int
}

// searchSpans are the port runs to try, nearest first. A privileged port gets
// one try at its own number — this process may be allowed to bind it — and
// then the whole search at its unprivileged twin, because scanning 80..144 as
// an ordinary user is 65 guaranteed failures on the way to an ephemeral port.
func searchSpans(want, search int) []searchSpan {
	if want < 1024 {
		return []searchSpan{{start: want, count: 1}, {start: want + privilegedOffset, count: search}}
	}
	return []searchSpan{{start: want, count: search}}
}
