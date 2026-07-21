// Package execstream defines the contract shared by the two ends of an exec
// attach stream: the client that lends a process its stdio, and the host that
// owns the process and serves attachers.
//
// The wire format itself is in execstream/frame. Everything between the two
// ends — the control plane's proxy, the sandbox-agent's websocket bridge — is a
// byte pipe that never decodes a frame, which is why only these two packages
// speak the protocol.
package execstream

import "github.com/obot-platform/discobox/execstream/frame"

// Conn is one duplex framed connection. It is the seam that makes the transport
// interchangeable: a websocket, a hijacked HTTP connection, a Unix socket, and
// net.Pipe are all the same to the code above it, which is what lets the
// protocol be exercised without a network.
//
// Implementations must be safe for one reader and one writer concurrently;
// WriteFrame may be called from several goroutines and must serialize writes
// itself.
type Conn interface {
	ReadFrame() (frame.Frame, error)
	WriteFrame(typ byte, payload []byte) error
	Close() error
}
