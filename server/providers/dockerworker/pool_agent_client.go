package dockerworker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"syscall"
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

// PoolAgentClient is a keep-alive client to one VM's pool agent. It is owned
// by the VM it dials, shared by every lease on that VM, and closed with Close
// when the VM stops or is replaced: a lease's release must not close it, or
// every request dials the guest again (#1335).
//
// It tracks every connection it has dialed, in use as well as idle, because
// nothing promises the host hears when a guest loses them: a vz guest that
// panics reboots inside the same VM, and the framework does not stop it. A
// dial the guest refuses or resets says its agent's listener is gone, so every
// connection made before it is dropped then, and a request still waiting on
// the old boot fails rather than waiting for its caller to give up. That
// includes a dial still in progress when the refusal came: it may have reached
// the old boot before it went, and is not yet tracked for Close to drop, so it
// is dropped as it completes (#1355).
type PoolAgentClient struct {
	*http.Client
	transport *http.Transport

	mu    sync.Mutex
	conns map[*poolAgentConn]struct{}
	// closes counts Close calls. A dial that saw a different count when it
	// started overlapped a Close, and its connection is dropped.
	closes uint64
}

// errPoolAgentDialClosed fails a dial that was in progress when the client
// closed: its connection may belong to a guest boot that is gone. track
// returns it as a dial *net.OpError, the failed dial it stands for, so callers
// that wait out an unanswered pool agent (poolruntime's poolAgentUnanswered)
// retry it like any other.
var errPoolAgentDialClosed = errors.New("pool agent client closed while dialing the guest")

// NewPoolAgentClient returns a keep-alive client whose connections come from
// dial.
func NewPoolAgentClient(dial func(context.Context) (net.Conn, error)) *PoolAgentClient {
	c := &PoolAgentClient{conns: map[*poolAgentConn]struct{}{}}
	c.transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			c.mu.Lock()
			closes := c.closes
			c.mu.Unlock()
			conn, err := dial(ctx)
			if err != nil {
				if guestRefused(err) {
					c.Close()
				}
				return nil, err
			}
			return c.track(conn, closes)
		},
		MaxIdleConns:        poolAgentIdleConns,
		MaxIdleConnsPerHost: poolAgentIdleConns,
		IdleConnTimeout:     poolAgentIdleTimeout,
	}
	c.Client = &http.Client{Transport: c.transport}
	return c
}

// Close drops every connection the client holds, idle or carrying a request.
// A request on a dropped connection fails rather than waiting on a guest that
// will never answer it. The client stays usable: the next request dials.
func (c *PoolAgentClient) Close() {
	c.transport.CloseIdleConnections()
	c.mu.Lock()
	conns := c.conns
	c.conns = map[*poolAgentConn]struct{}{}
	c.closes++
	c.mu.Unlock()
	for conn := range conns {
		_ = conn.Conn.Close()
	}
}

// track records conn, dialed when the client had closed closes times, so a
// later Close drops it. A Close since then may have come too late to see it,
// so conn is closed and the dial fails instead.
func (c *PoolAgentClient) track(conn net.Conn, closes uint64) (net.Conn, error) {
	c.mu.Lock()
	if c.closes != closes {
		c.mu.Unlock()
		_ = conn.Close()
		return nil, &net.OpError{Op: "dial", Err: errPoolAgentDialClosed}
	}
	tracked := &poolAgentConn{Conn: conn, owner: c}
	c.conns[tracked] = struct{}{}
	c.mu.Unlock()
	return tracked, nil
}

// guestRefused reports a dial the guest answered with no listener: the agent
// it reached before is gone, along with every connection made to it.
func guestRefused(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED)
}

// poolAgentConn removes itself from its client when the transport closes it.
// It deliberately embeds net.Conn alone, so it exposes no CloseWrite the
// underlying connection might not honor.
type poolAgentConn struct {
	net.Conn
	owner *PoolAgentClient
}

func (c *poolAgentConn) Close() error {
	c.owner.mu.Lock()
	delete(c.owner.conns, c)
	c.owner.mu.Unlock()
	return c.Conn.Close()
}
