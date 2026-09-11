package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/discobox-ai/discobox/cli/internal/portforward"
	"github.com/discobox-ai/discobox/execstream/frame"
)

// tunnelDialTimeout bounds the handshake only. Once the tunnel is up it lives
// as long as the forwarded connection, or flow, does.
const tunnelDialTimeout = 30 * time.Second

// sandboxPortDialer opens a connection to a port inside a sandbox over one of
// the control plane's tunnel websockets: `/tcp/attach` for a TCP port — the
// same tunnel `ssh -L` gets (ADR 0024 §3), reached over the transport the API
// already answers on rather than through an SSH session — and `/udp/attach` for
// a UDP one (ADR 0109 §4).
//
// It is a portforward.Dialer, which is all the forwarder knows about it.
type sandboxPortDialer struct {
	baseURL   string
	client    *http.Client
	projectID string
	sandboxID string
}

func (a *App) sandboxPortDialer(projectID, sandboxID string) (*sandboxPortDialer, error) {
	baseURL, client, err := a.httpClient()
	if err != nil {
		return nil, err
	}
	return &sandboxPortDialer{baseURL: baseURL, client: client, projectID: projectID, sandboxID: sandboxID}, nil
}

func (d *sandboxPortDialer) DialPort(ctx context.Context, target portforward.Target) (net.Conn, error) {
	socketURL, err := sandboxTunnelWebSocketURL(d.baseURL, d.projectID, d.sandboxID, target)
	if err != nil {
		return nil, err
	}
	// The tunnel outlives the handshake, so the conn keeps a context of its
	// own descended from the caller's: canceling the handshake's timeout must
	// not cut the connection it opened.
	connCtx, cancel := context.WithCancel(ctx)
	dialCtx, cancelDial := context.WithTimeout(connCtx, tunnelDialTimeout)
	defer cancelDial()

	wsConn, resp, err := websocket.Dial(dialCtx, socketURL, &websocket.DialOptions{HTTPClient: d.client})
	if err != nil {
		cancel()
		return nil, tunnelDialError(target, resp, err)
	}
	if resp != nil && resp.Body != nil {
		// The handshake response body carries nothing once the connection is
		// upgraded, but leaving it open leaks the underlying connection.
		_ = resp.Body.Close()
	}
	// The frame payloads the sandbox-agent sends run to the full size of its
	// read buffer, which is the library's default message limit exactly for
	// TCP and twice it for a UDP datagram. There is no useful ceiling on a
	// byte pipe, so there is none here.
	wsConn.SetReadLimit(-1)
	return &tunnelConn{
		ws:        wsConn,
		stream:    websocket.NetConn(connCtx, wsConn, websocket.MessageBinary),
		cancel:    cancel,
		target:    target,
		datagrams: target.Network == portforward.UDP,
	}, nil
}

// tunnelDialError says why the tunnel was refused. The reason only exists in
// the handshake response body — a websocket dial error is "expected 101" and
// nothing more — and it is gone once the response is discarded.
func tunnelDialError(target portforward.Target, resp *http.Response, err error) error {
	where := net.JoinHostPort(target.Host, strconv.Itoa(target.Port))
	if target.Network == portforward.UDP {
		where += "/udp"
	}
	if resp == nil || resp.Body == nil {
		return fmt.Errorf("connect to %s: %w", where, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	switch message := tunnelErrorMessage(body); message {
	case "":
		return fmt.Errorf("connect to %s: %s", where, resp.Status)
	default:
		return fmt.Errorf("connect to %s: %s", where, message)
	}
}

// tunnelErrorMessage unwraps the {"error": "..."} body every hop in the chain
// answers with, so the line a user reads is the reason rather than the
// envelope it arrived in.
func tunnelErrorMessage(body []byte) string {
	var wrapped struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && strings.TrimSpace(wrapped.Error) != "" {
		return strings.TrimSpace(wrapped.Error)
	}
	return strings.TrimSpace(string(body))
}

// sandboxTunnelWebSocketURL is the control-plane route that dials the target's
// host:port from inside the sandbox's network namespace, over the target's
// network.
func sandboxTunnelWebSocketURL(baseURL, projectID, sandboxID string, target portforward.Target) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return "", fmt.Errorf("parse server URL %q: %w", baseURL, err)
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	default:
		parsed.Scheme = "ws"
	}
	route := "/tcp/attach"
	if target.Network == portforward.UDP {
		route = "/udp/attach"
	}
	parsed.Path = "/api/projects/" + url.PathEscape(projectID) + "/sandboxes/" + url.PathEscape(sandboxID) + route
	parsed.RawQuery = url.Values{
		"host": {target.Host},
		"port": {strconv.Itoa(target.Port)},
	}.Encode()
	return parsed.String(), nil
}

// tunnelConn presents the framed tunnel as an ordinary net.Conn, so everything
// above it is a plain TCP proxy — or, for a UDP tunnel, a connected UDP socket.
//
// The framing is not decoration. For TCP it is what carries a half-close
// across the websocket (ADR 0024 §4): Write becomes an Input frame, CloseWrite
// a CloseInput frame, and only Stdout frames come back as bytes. For UDP it is
// what keeps a datagram one datagram (ADR 0109 §4): each Write is one Input
// frame, and each Read returns one Stdout frame and no more.
type tunnelConn struct {
	ws     *websocket.Conn
	stream net.Conn
	cancel context.CancelFunc
	target portforward.Target
	// datagrams makes Read return one frame per call, the way a UDP socket
	// returns one datagram, instead of a byte stream that runs frames
	// together.
	datagrams bool

	// writeMu orders the header and payload of a frame against any other
	// writer. A websocket write is one message, and two interleaved frames
	// would be unreadable.
	writeMu sync.Mutex

	// pending is what is left of the last Stdout frame, since a frame is not
	// obliged to fit the caller's buffer.
	pending []byte

	closeOnce sync.Once
}

func (c *tunnelConn) Read(p []byte) (int, error) {
	if c.datagrams {
		return c.readDatagram(p)
	}
	for len(c.pending) == 0 {
		read, err := frame.Read(c.stream)
		if err != nil {
			return 0, err
		}
		switch read.Type {
		case frame.Stdout:
			c.pending = read.Payload
		case frame.CloseOutput:
			// The far end is done sending. That is this conn's EOF, and it
			// says nothing about whether it can still receive: a caller with
			// more to write goes on writing.
			return 0, io.EOF
		case frame.Error:
			return 0, fmt.Errorf("discobox tunnel: %s", strings.TrimSpace(string(read.Payload)))
		default:
			// A byte pipe has no exit status, no resize and no stderr; a frame
			// that is none of the above is not for this conn.
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

// readDatagram is Read for a UDP tunnel: the next Stdout frame, whole. A
// datagram longer than p is truncated, as a UDP socket truncates one, rather
// than carried into the next Read, where it would read as a datagram the target
// never sent.
func (c *tunnelConn) readDatagram(p []byte) (int, error) {
	for {
		read, err := frame.Read(c.stream)
		if err != nil {
			return 0, err
		}
		switch read.Type {
		case frame.Stdout:
			return copy(p, read.Payload), nil
		case frame.CloseOutput:
			return 0, io.EOF
		case frame.Error:
			return 0, fmt.Errorf("discobox tunnel: %s", strings.TrimSpace(string(read.Payload)))
		}
	}
}

func (c *tunnelConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := frame.Write(c.stream, frame.Input, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// CloseWrite ends this side's half of the connection without ending the other,
// which is how a client that streams a request until EOF gets a response.
func (c *tunnelConn) CloseWrite() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return frame.Write(c.stream, frame.CloseInput, nil)
}

func (c *tunnelConn) Close() error {
	c.closeOnce.Do(func() {
		_ = c.ws.Close(websocket.StatusNormalClosure, "done")
		_ = c.stream.Close()
		c.cancel()
	})
	return nil
}

func (c *tunnelConn) LocalAddr() net.Addr  { return c.stream.LocalAddr() }
func (c *tunnelConn) RemoteAddr() net.Addr { return c.stream.RemoteAddr() }

func (c *tunnelConn) SetDeadline(t time.Time) error      { return c.stream.SetDeadline(t) }
func (c *tunnelConn) SetReadDeadline(t time.Time) error  { return c.stream.SetReadDeadline(t) }
func (c *tunnelConn) SetWriteDeadline(t time.Time) error { return c.stream.SetWriteDeadline(t) }
