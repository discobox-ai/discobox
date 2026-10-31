package cpchannel

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/cpmux"
)

// streamWait is how long a local connection waits for a channel when there is
// none: long enough to ride out the server redialing a dropped one, short
// enough that a caller sees an unreachable control plane rather than a hang.
const streamWait = 30 * time.Second

// Channel is the pool end: the session the server's latest channel carries,
// and the socket the pool's own processes reach the control plane at.
//
// A newer channel replaces an older one, which is closed: there is one server,
// and the one that dialed last is the one that is there.
type Channel struct {
	tls    *tls.Config
	logger *slog.Logger

	mu      sync.Mutex
	session *cpmux.Session
	// arrived is closed when a session is installed, and replaced by a fresh
	// one, so a waiter wakes for each new session.
	arrived chan struct{}
}

// New is the pool end of a channel for the pool whose key is poolKey, which
// accepts only the control plane whose issuer key is controlPlane.
func New(logger *slog.Logger, poolKey ed25519.PrivateKey, controlPlane ed25519.PublicKey) (*Channel, error) {
	if logger == nil {
		logger = slog.Default()
	}
	config, err := poolTLS(poolKey, controlPlane)
	if err != nil {
		return nil, err
	}
	return &Channel{
		tls:     config,
		logger:  logger,
		arrived: make(chan struct{}),
	}, nil
}

// ServeHTTP accepts the server's channel. It must sit behind the agent's
// authentication of server requests, holding the request to the scope that
// opens a channel: the
// pinned TLS proves the far end, the token proves it asked for this.
func (c *Channel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := accept(w, r, c.tls)
	if err != nil {
		c.logger.Warn("control-plane channel refused", "error", err)
		return
	}
	session, err := cpmux.Server(conn)
	if err != nil {
		_ = conn.Close()
		c.logger.Warn("control-plane channel refused", "error", err)
		return
	}
	c.install(session)
	c.logger.Info("control-plane channel open")
	go c.refuseStreams(session)
}

// refuseStreams closes what the server opens toward the pool on a channel:
// it reaches the agent's API at its own URL, so a stream it opens here is
// not one it means. It also ends with the session, which is what notices it
// went away.
func (c *Channel) refuseStreams(session *cpmux.Session) {
	for {
		conn, _, err := session.Accept()
		if err != nil {
			c.logger.Info("control-plane channel closed", "error", err)
			return
		}
		_ = conn.Close()
	}
}

func (c *Channel) install(session *cpmux.Session) {
	c.mu.Lock()
	old := c.session
	c.session = session
	close(c.arrived)
	c.arrived = make(chan struct{})
	c.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// Wait returns once a channel is open, or when ctx ends.
func (c *Channel) Wait(ctx context.Context) error {
	_, err := c.current(ctx)
	return err
}

// current is the live session, waiting for one while there is none.
func (c *Channel) current(ctx context.Context) (*cpmux.Session, error) {
	for {
		c.mu.Lock()
		session, arrived := c.session, c.arrived
		c.mu.Unlock()
		if session != nil && !session.Closed() {
			return session, nil
		}
		select {
		case <-arrived:
		case <-ctx.Done():
			return nil, errors.New("no control-plane channel: the server has not opened one to this pool")
		}
	}
}

// Serve hands every connection listener accepts to the control plane, as a
// stream on the current channel. It is how the agent and the proxy unit reach
// the control plane: a unix:// URL each dials with pkg/wire, knowing nothing
// of the channel behind it. It returns when listener is closed.
func (c *Channel) Serve(ctx context.Context, listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go c.forward(ctx, conn)
	}
}

func (c *Channel) forward(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	wait, cancel := context.WithTimeout(ctx, streamWait)
	session, err := c.current(wait)
	if err != nil {
		cancel()
		c.logger.Warn("control-plane connection dropped", "error", err)
		return
	}
	stream, err := session.Dial(wait, cpmux.TargetControlPlane)
	cancel()
	if err != nil {
		c.logger.Warn("control-plane connection dropped", "error", err)
		return
	}
	defer func() { _ = stream.Close() }()
	cpmux.Splice(conn, stream)
}
