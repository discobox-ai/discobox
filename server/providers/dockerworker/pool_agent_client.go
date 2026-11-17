package dockerworker

import (
	"context"
	"net"
	"net/http"
	"time"
)

// Keep-alive policy for a client that reaches a pool agent through a VM's
// VSOCK device, where every dial is a new connection into the guest.
const (
	// poolAgentIdleConns covers the concurrent requests a pool sees in steady
	// state (each attached console polls two inspect reads at once, plus one
	// read per running box), so a burst returns its connections to the pool
	// rather than closing all but two of them.
	poolAgentIdleConns = 16
	// poolAgentIdleTimeout is below the guest agent's own idle timeout, so the
	// client drops a connection before the guest does, and bounds how long a
	// stalled guest's connections are held.
	poolAgentIdleTimeout = 30 * time.Second
)

// NewPoolAgentClient returns a keep-alive client whose connections come from
// dial. It is owned by the VM it dials, shared by every lease on that VM, and
// closed (CloseIdleConnections) when the VM stops or is replaced: a lease's
// release must not close it, or every request dials the guest again.
func NewPoolAgentClient(dial func(context.Context) (net.Conn, error)) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx)
		},
		MaxIdleConns:        poolAgentIdleConns,
		MaxIdleConnsPerHost: poolAgentIdleConns,
		IdleConnTimeout:     poolAgentIdleTimeout,
	}}
}
