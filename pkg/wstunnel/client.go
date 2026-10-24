package wstunnel

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Client opens streams to one tunnel URL, starting each on an idle tunnel when
// it holds one and on a new tunnel otherwise.
type Client struct {
	url  string
	http *http.Client
	// limits are taken when the client is made.
	limits limits

	mu   sync.Mutex
	idle []*tunnel
}

// NewClient returns a client for url, a ws or wss URL. httpClient carries the
// WebSocket handshake; nil uses one that verifies against the system roots and
// never goes through a proxy, since the caller is what a sandbox's proxy
// traffic rides on.
func NewClient(url string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = directHTTPClient()
	}
	return &Client{url: url, http: httpClient, limits: currentLimits()}
}

func directHTTPClient() *http.Client {
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	// A WebSocket handshake is HTTP/1.1: a connection that negotiated h2 has
	// nothing to upgrade.
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	return &http.Client{Transport: transport}
}

// Dial starts a stream, on an idle tunnel when there is one. A stream on a
// reused tunnel says so (Stream.Reused): the caller that finds it dead on
// first use asks DialNew for the one retry.
func (c *Client) Dial(ctx context.Context) (*Stream, error) {
	for {
		t := c.takeIdle()
		if t == nil {
			return c.DialNew(ctx)
		}
		if s := t.start(true); s != nil {
			return s, nil
		}
	}
}

// takeIdle removes the most recently idled tunnel from the idle set, or
// returns nil when there is none.
func (c *Client) takeIdle() *tunnel {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.idle) > 0 {
		t := c.idle[len(c.idle)-1]
		c.idle = c.idle[:len(c.idle)-1]
		// A timer that already fired is retiring the tunnel.
		if t.idleTimer.Stop() {
			return t
		}
	}
	return nil
}

// DialNew starts a stream on a new tunnel.
func (c *Client) DialNew(ctx context.Context) (*Stream, error) {
	ws, resp, err := websocket.Dial(ctx, c.url, &websocket.DialOptions{
		HTTPClient:      c.http,
		Subprotocols:    []string{Subprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("open tunnel to %s: %w", c.url, err)
	}
	if ws.Subprotocol() != Subprotocol {
		_ = ws.CloseNow()
		return nil, fmt.Errorf("open tunnel to %s: the server does not speak %s", c.url, Subprotocol)
	}
	// The tunnel outlives the dial's context: it is reused by later streams.
	t := newTunnel(context.Background(), c.limits, ws, false, addr("wss"), addr(c.url))
	t.onIdle = c.release
	t.onDead = c.remove
	// The stream is claimed before the reader starts: a tunnel the server
	// closes at once fails the stream rather than leaving nothing to claim.
	s := t.start(false)
	go t.run()
	go t.keepalive()
	return s, nil
}

// release takes back a tunnel whose stream ended cleanly, or retires it when
// it is too old to reuse or the client holds enough.
func (c *Client) release(t *tunnel) {
	c.mu.Lock()
	if time.Since(t.born) >= c.limits.maxAge || len(c.idle) >= c.limits.maxIdle {
		c.mu.Unlock()
		// Retiring calls remove, which takes the lock.
		t.retire()
		return
	}
	defer c.mu.Unlock()
	t.idleTimer = time.AfterFunc(c.limits.idleTimeout, func() {
		c.mu.Lock()
		c.idle = slices.DeleteFunc(c.idle, func(o *tunnel) bool { return o == t })
		c.mu.Unlock()
		t.retire()
	})
	c.idle = append(c.idle, t)
}

// remove forgets a tunnel that stopped.
func (c *Client) remove(t *tunnel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.idleTimer != nil {
		t.idleTimer.Stop()
	}
	c.idle = slices.DeleteFunc(c.idle, func(o *tunnel) bool { return o == t })
}

// CloseIdle retires every idle tunnel.
func (c *Client) CloseIdle() {
	c.mu.Lock()
	idle := c.idle
	c.idle = nil
	c.mu.Unlock()
	for _, t := range idle {
		t.idleTimer.Stop()
		t.retire()
	}
}
