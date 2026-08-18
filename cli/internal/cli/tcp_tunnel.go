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

	"github.com/obot-platform/discobox/cli/internal/portforward"
	"github.com/obot-platform/discobox/execstream/frame"
)

// tcpTunnelDialTimeout bounds the handshake only. Once the tunnel is up it
// lives as long as the forwarded connection does.
const tcpTunnelDialTimeout = 30 * time.Second

// sandboxTCPDialer opens a connection to a port inside a sandbox over the
// control plane's `/tcp/attach` websocket — the same tunnel `ssh -L` gets
// (ADR 0024 §3), reached over the transport the API already answers on rather
// than through an SSH session.
//
// It is a portforward.Dialer, which is all the forwarder knows about it.
type sandboxTCPDialer struct {
	baseURL   string
	client    *http.Client
	projectID string
	sandboxID string
}

func (a *App) sandboxTCPDialer(projectID, sandboxID string) (*sandboxTCPDialer, error) {
	baseURL, client, err := a.httpClient()
	if err != nil {
		return nil, err
	}
	return &sandboxTCPDialer{baseURL: baseURL, client: client, projectID: projectID, sandboxID: sandboxID}, nil
}

func (d *sandboxTCPDialer) DialPort(ctx context.Context, target portforward.Target) (net.Conn, error) {
	socketURL, err := sandboxTCPWebSocketURL(d.baseURL, d.projectID, d.sandboxID, target.Host, target.Port)
	if err != nil {
		return nil, err
	}
	// The tunnel outlives the handshake, so the conn keeps a context of its
	// own descended from the caller's: canceling the handshake's timeout must
	// not cut the connection it opened.
	connCtx, cancel := context.WithCancel(ctx)
	dialCtx, cancelDial := context.WithTimeout(connCtx, tcpTunnelDialTimeout)
	defer cancelDial()

	wsConn, resp, err := websocket.Dial(dialCtx, socketURL, &websocket.DialOptions{HTTPClient: d.client})
	if err != nil {
		cancel()
		return nil, tcpTunnelDialError(target, resp, err)
	}
	if resp != nil && resp.Body != nil {
		// The handshake response body carries nothing once the connection is
		// upgraded, but leaving it open leaks the underlying connection.
		_ = resp.Body.Close()
	}
	// The frame payloads the sandbox-agent sends run to the full size of its
	// read buffer, which is the library's default message limit exactly. There
	// is no useful ceiling on a byte pipe, so there is none here.
	wsConn.SetReadLimit(-1)
	return &tcpTunnelConn{
		ws:     wsConn,
		stream: websocket.NetConn(connCtx, wsConn, websocket.MessageBinary),
		cancel: cancel,
		target: target,
	}, nil
}

// tcpTunnelDialError says why the tunnel was refused. The reason only exists
// in the handshake response body — a websocket dial error is "expected 101" and
// nothing more — and it is gone once the response is discarded.
func tcpTunnelDialError(target portforward.Target, resp *http.Response, err error) error {
	where := net.JoinHostPort(target.Host, strconv.Itoa(target.Port))
	if resp == nil || resp.Body == nil {
		return fmt.Errorf("connect to %s: %w", where, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	switch message := tcpTunnelErrorMessage(body); message {
	case "":
		return fmt.Errorf("connect to %s: %s", where, resp.Status)
	default:
		return fmt.Errorf("connect to %s: %s", where, message)
	}
}

// tcpTunnelErrorMessage unwraps the {"error": "..."} body every hop in the
// chain answers with, so the line a user reads is the reason rather than the
// envelope it arrived in.
func tcpTunnelErrorMessage(body []byte) string {
	var wrapped struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && strings.TrimSpace(wrapped.Error) != "" {
		return strings.TrimSpace(wrapped.Error)
	}
	return strings.TrimSpace(string(body))
}

// sandboxTCPWebSocketURL is the control-plane route that dials host:port from
// inside the sandbox's network namespace.
func sandboxTCPWebSocketURL(baseURL, projectID, sandboxID, host string, port int) (string, error) {
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
	parsed.Path = "/api/projects/" + url.PathEscape(projectID) + "/sandboxes/" + url.PathEscape(sandboxID) + "/tcp/attach"
	parsed.RawQuery = url.Values{
		"host": {host},
		"port": {strconv.Itoa(port)},
	}.Encode()
	return parsed.String(), nil
}

// tcpTunnelConn presents the framed tunnel as an ordinary net.Conn, so
// everything above it is a plain TCP proxy.
//
// The framing is not decoration: it is what carries a half-close across the
// websocket (ADR 0024 §4). Write becomes an Input frame, CloseWrite a
// CloseInput frame, and only Stdout frames come back as bytes.
type tcpTunnelConn struct {
	ws     *websocket.Conn
	stream net.Conn
	cancel context.CancelFunc
	target portforward.Target

	// writeMu orders the header and payload of a frame against any other
	// writer. A websocket write is one message, and two interleaved frames
	// would be unreadable.
	writeMu sync.Mutex

	// pending is what is left of the last Stdout frame, since a frame is not
	// obliged to fit the caller's buffer.
	pending []byte

	closeOnce sync.Once
}

func (c *tcpTunnelConn) Read(p []byte) (int, error) {
	for len(c.pending) == 0 {
		read, err := frame.Read(c.stream)
		if err != nil {
			return 0, err
		}
		switch read.Type {
		case frame.Stdout:
			c.pending = read.Payload
		case frame.Error:
			return 0, fmt.Errorf("sandbox tunnel: %s", strings.TrimSpace(string(read.Payload)))
		default:
			// A byte pipe has no exit status, no resize and no stderr; a frame
			// that is none of the above is not for this conn.
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *tcpTunnelConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := frame.Write(c.stream, frame.Input, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// CloseWrite ends this side's half of the connection without ending the other,
// which is how a client that streams a request until EOF gets a response.
func (c *tcpTunnelConn) CloseWrite() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return frame.Write(c.stream, frame.CloseInput, nil)
}

func (c *tcpTunnelConn) Close() error {
	c.closeOnce.Do(func() {
		_ = c.ws.Close(websocket.StatusNormalClosure, "done")
		_ = c.stream.Close()
		c.cancel()
	})
	return nil
}

func (c *tcpTunnelConn) LocalAddr() net.Addr  { return c.stream.LocalAddr() }
func (c *tcpTunnelConn) RemoteAddr() net.Addr { return c.stream.RemoteAddr() }

func (c *tcpTunnelConn) SetDeadline(t time.Time) error      { return c.stream.SetDeadline(t) }
func (c *tcpTunnelConn) SetReadDeadline(t time.Time) error  { return c.stream.SetReadDeadline(t) }
func (c *tcpTunnelConn) SetWriteDeadline(t time.Time) error { return c.stream.SetWriteDeadline(t) }
