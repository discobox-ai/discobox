package ports

import (
	"context"
	"net/netip"
)

// Scanner is the observation seam for ports (ADR 0145 §4): it lists the sockets
// a server could be behind — listening TCP sockets and bound, unconnected UDP
// ones — that the sandbox's own user holds. Every platform the sandbox agent
// runs on has one, chosen by platformScanner, and the rest of the package does
// not know which it is talking to.
//
// It is sealed: the implementations are this package's, one per platform, and
// a caller picks one rather than writing another.
type Scanner interface {
	scan(ctx context.Context, uid int64) ([]listener, error)
}

// listener is one socket a server could be behind, as a Scanner reports it.
type listener struct {
	Network network
	Addr    netip.Addr
	Port    int
	// Socket identifies the socket itself, so a server that restarts on the
	// same port is seen to have a new one: its inode on Linux, its kernel
	// address on darwin.
	Socket uint64
}

// network is which table a socket came from. A TCP port and a UDP port with the
// same number are two ports (ADR 0109 §1), so it is half of every key.
type network string

const (
	networkTCP network = "tcp"
	networkUDP network = "udp"
)

// portRange is an inclusive range of port numbers.
//
// Every Scanner drops a UDP socket whose port is inside the ephemeral range:
// that is where the kernel puts a client's socket when it binds nothing, and
// an unconnected client looks exactly like a server in every other respect
// (ADR 0109 §1).
type portRange struct{ low, high int }

func (r portRange) contains(port int) bool { return port >= r.low && port <= r.high }
