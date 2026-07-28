// Package carrierhub serves the control-plane HTTP API over connections that
// the server itself opened, instead of connections it accepted from a port.
//
// Pool backends whose transport only dials inward — today the Windows wslc
// provider, whose bridge reaches into a VM — cannot have their in-guest agent
// dial the control plane. Rather than making the server listen on TCP (which on
// Windows means a firewall prompt), such a driver opens connections into its
// guest and hands them here. The Hub presents them as a net.Listener, so the
// ordinary control-plane handler, routing, and authentication serve them
// unchanged; only which side performed the connect differs.
package carrierhub

import (
	"errors"
	"net"
	"sync"
)

var (
	// ErrClosed is returned by Push after the Hub is closed.
	ErrClosed = errors.New("carrier hub is closed")
	// ErrPushCanceled is returned by Push when its cancel channel fires before
	// the server accepts the connection. It is deliberately not
	// context.Canceled: Push takes a plain channel so a driver can hand it its
	// own lifetime without constructing a context per connection.
	ErrPushCanceled = errors.New("carrier push canceled")
)

// Hub is a net.Listener fed by Push rather than by an operating system
// listener. It is safe for concurrent use.
type Hub struct {
	conns chan net.Conn

	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

// New creates an empty Hub.
func New() *Hub {
	return &Hub{
		conns: make(chan net.Conn),
		done:  make(chan struct{}),
	}
}

// Push hands a connection to the server. It blocks until the server accepts the
// connection, the Hub closes, or cancel fires, so a driver cannot build an
// unbounded backlog of guest connections. A canceled or rejected connection is
// left for the caller to close.
func (h *Hub) Push(conn net.Conn, cancel <-chan struct{}) error {
	if conn == nil {
		return errors.New("carrier connection is required")
	}
	select {
	case <-h.done:
		return ErrClosed
	default:
	}
	select {
	case h.conns <- conn:
		return nil
	case <-h.done:
		return ErrClosed
	case <-cancel:
		return ErrPushCanceled
	}
}

// Accept implements net.Listener. It blocks until a driver pushes a connection.
func (h *Hub) Accept() (net.Conn, error) {
	select {
	case conn := <-h.conns:
		return conn, nil
	case <-h.done:
		// net/http treats a listener error as fatal unless it is temporary, so
		// returning ErrClosed here is what stops Serve cleanly at shutdown.
		return nil, net.ErrClosed
	}
}

// Close stops the Hub. Connections already handed to the server are unaffected;
// the server closes them as it finishes with them.
func (h *Hub) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	close(h.done)
	return nil
}

// Addr implements net.Listener. The Hub has no address of its own: every
// connection originates from a driver's own transport.
func (h *Hub) Addr() net.Addr { return hubAddr{} }

type hubAddr struct{}

func (hubAddr) Network() string { return "carrier" }
func (hubAddr) String() string  { return "carrier-hub" }
