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
//
// Every listen error is treated as "try the next one" rather than matched
// against EADDRINUSE: the outcomes worth telling apart here are "got a port"
// and "got none", and matching errno per platform buys nothing. If the whole
// search fails, any port at all beats no forward — the caller prints the
// number it got — and only if that fails too does the first error surface.
func listenNearest(ctx context.Context, address string, want, search int) (net.Listener, error) {
	var config net.ListenConfig
	var firstErr error
	listen := func(port int) net.Listener {
		listener, err := config.Listen(ctx, "tcp", net.JoinHostPort(address, strconv.Itoa(port)))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return nil
		}
		return listener
	}

	for _, span := range searchSpans(want, search) {
		for port := span.start; port < span.start+span.count && port <= 65535; port++ {
			if listener := listen(port); listener != nil {
				return listener, nil
			}
		}
	}
	if listener := listen(0); listener != nil {
		return listener, nil
	}
	return nil, firstErr
}

// listenExact binds want and nothing else, for a forward whose local number is
// not the caller's to choose — see Options.Exact.
func listenExact(ctx context.Context, address string, want int) (net.Listener, error) {
	var config net.ListenConfig
	return config.Listen(ctx, "tcp", net.JoinHostPort(address, strconv.Itoa(want)))
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
