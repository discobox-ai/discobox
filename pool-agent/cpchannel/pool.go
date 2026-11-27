package cpchannel

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/cpmux"
	"github.com/discobox-ai/discobox/pool-agent/handover"
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
	// forwarding counts the socket's connections that are new or in a
	// request (busy), and the requests being forwarded onto a session, an
	// upgraded one's whole life included (Drain).
	forwarding handover.InFlight
	busy       map[net.Conn]func()
	// server serves the socket (Serve), for Drain to stop keeping its
	// connections alive.
	server *http.Server
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

// Drain closes the channel once every request already forwarded onto it has
// its whole answer, or when ctx ends, whichever is first: what a draining
// agent does once it has stopped accepting its socket (ADR 26-11-26-536 §4).
// The socket's connections stop being kept alive first: an idle one closes at
// once, and one in a request, its request still arriving included, closes
// once answered; a streamed answer or an upgraded connection runs to its end
// or ctx's. The server then dials a
// channel again, which reaches the agent that took over, the only one still
// accepting; a draining agent's own calls to the control plane reach it
// through the socket that agent now serves. Until then, the agent that took
// over and its proxy reach no control plane, so the drain is kept short.
func (c *Channel) Drain(ctx context.Context) {
	c.mu.Lock()
	server := c.server
	c.mu.Unlock()
	if server != nil {
		// An idle connection closes at once, and one in a request closes
		// once answered. Not Shutdown: a request still arriving as it
		// begins would be closed unanswered.
		server.SetKeepAlivesEnabled(false)
	}
	if err := c.forwarding.Wait(ctx); err != nil {
		c.logger.Warn("closing the control-plane channel with requests still on it", "error", err)
	}
	c.mu.Lock()
	session := c.session
	c.mu.Unlock()
	if session != nil {
		_ = session.Close()
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

// Serve forwards every request on a connection listener accepts to the
// control plane, on a stream of the current channel. It is how the agent and
// the proxy unit reach the control plane: a unix:// URL each dials with
// pkg/wire, knowing nothing of the channel behind it. Each is served as HTTP,
// so a drain knows a connection between requests from one in a request, and
// each request is its own stream, so one in flight when a newer channel
// replaces the old keeps the stream it began on. It returns when listener is
// closed.
func (c *Channel) Serve(ctx context.Context, listener net.Listener) error {
	server := &http.Server{
		Handler:           c.forwarder(),
		ReadHeaderTimeout: 30 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ConnState:         c.connState,
	}
	c.mu.Lock()
	c.server = server
	c.mu.Unlock()
	err := server.Serve(listener)
	if errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// connState counts a connection of the socket while it is new or in a
// request, so a drain waits for a request still arriving as well as one
// being answered.
func (c *Channel) connState(conn net.Conn, state http.ConnState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	end, busy := c.busy[conn]
	switch state {
	case http.StateNew, http.StateActive:
		if !busy {
			if c.busy == nil {
				c.busy = make(map[net.Conn]func())
			}
			c.busy[conn] = c.forwarding.Begin()
		}
	default:
		if busy {
			end()
			delete(c.busy, conn)
		}
	}
}

// forwarder sends each request to the control plane on a stream of the
// current channel, answering 502 when there is none within streamWait. An
// answer is passed on as it arrives, so a streamed one is not held back, and
// an upgrade is spliced for its whole life.
func (c *Channel) forwarder() http.Handler {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			wait, cancel := context.WithTimeout(ctx, streamWait)
			defer cancel()
			session, err := c.current(wait)
			if err != nil {
				return nil, err
			}
			return session.Dial(wait, cpmux.TargetControlPlane)
		},
		// A stream per request: one kept for another would outlive the
		// channel it belongs to, which a newer one replaces.
		DisableKeepAlives:  true,
		DisableCompression: true,
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = cpmux.TargetControlPlane
			r.Out.Host = r.In.Host
		},
		Transport:     transport,
		FlushInterval: -1,
		// A request the control plane never answered is dropped as the
		// splice before this dropped it: the caller sees an unreachable
		// control plane, never an answer it did not give.
		ErrorHandler: func(_ http.ResponseWriter, r *http.Request, err error) {
			c.logger.Warn("control-plane request dropped", "path", r.URL.Path, "error", err)
			panic(http.ErrAbortHandler)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer c.forwarding.Begin()()
		proxy.ServeHTTP(w, r)
	})
}
