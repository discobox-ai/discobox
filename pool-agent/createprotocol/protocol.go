// Package createprotocol binds create-protocol discovery and dispatch to one
// agent process over one WebSocket. A reconnect always requires fresh discovery.
package createprotocol

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Revision advances whenever the required create contract changes.
const Revision = 1
const Subprotocol = "discobox-create.v1"

type Discovery struct {
	CreateRevision int `json:"createRevision"`
}

func Path(projectID, poolID string) string {
	return "/api/project/" + url.PathEscape(projectID) + "/pool/" + url.PathEscape(poolID) + "/create-protocol"
}

// Dial discovers the protocol on the same stream returned for dispatch. HTTP
// status is retained for legacy-route, readiness, and authentication handling.
func Dial(ctx context.Context, client *http.Client, baseURL, projectID, poolID, token string) (*http.Client, func(), int, error) {
	// Acquisition's deadline bounds discovery, not the returned lease's life.
	// net/http keeps an upgraded response bound to its handshake context.
	streamCtx, cancelStream := context.WithCancel(context.WithoutCancel(ctx))
	stopDiscovery := context.AfterFunc(ctx, cancelStream)
	defer stopDiscovery()
	transport := http.DefaultTransport
	if client != nil && client.Transport != nil {
		transport = client.Transport
	}
	if t, ok := transport.(*http.Transport); ok {
		t = t.Clone()
		var protocols http.Protocols
		protocols.SetHTTP1(true)
		t.Protocols = &protocols
		t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		if t.TLSClientConfig != nil {
			t.TLSClientConfig = t.TLSClientConfig.Clone()
			t.TLSClientConfig.NextProtos = []string{"http/1.1"}
		}
		defer t.CloseIdleConnections()
		transport = t
	}
	ws, resp, err := websocket.Dial(streamCtx, strings.TrimRight(baseURL, "/")+Path(projectID, poolID), &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: transport}, HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}, Subprotocols: []string{Subprotocol}, CompressionMode: websocket.CompressionDisabled,
	})
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	if err != nil {
		cancelStream()
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, nil, status, err
	}
	closeStream := func() { cancelStream(); _ = ws.CloseNow() }
	fail := func(err error) (*http.Client, func(), int, error) { closeStream(); return nil, nil, status, err }
	if ws.Subprotocol() != Subprotocol {
		return fail(errors.New("pool create discovery: missing subprotocol"))
	}
	ws.SetReadLimit(4096)
	kind, data, err := ws.Read(ctx)
	if err != nil {
		if websocket.CloseStatus(err) == websocket.StatusMessageTooBig {
			return fail(fmt.Errorf("pool create discovery: oversized envelope: %w", err))
		}
		return fail(&ConnectionLost{Err: err})
	}
	var discovery Discovery
	if kind != websocket.MessageText {
		return fail(errors.New("pool create discovery: expected JSON envelope"))
	}
	if err = json.Unmarshal(data, &discovery); err != nil || discovery.CreateRevision <= 0 {
		return fail(errors.New("pool create discovery: malformed revision"))
	}
	if discovery.CreateRevision != Revision {
		return fail(&Incompatible{Revision: discovery.CreateRevision})
	}
	stopDiscovery()
	if ctx.Err() != nil {
		return fail(&ConnectionLost{Err: ctx.Err()})
	}
	conn := websocket.NetConn(streamCtx, ws, websocket.MessageBinary)
	var mu sync.Mutex
	used := false
	t := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		if used {
			return nil, errors.New("pool create stream closed; fresh discovery required")
		}
		used = true
		return conn, nil
	}}
	return &http.Client{Transport: t}, func() { t.CloseIdleConnections(); closeStream() }, status, nil
}

type Incompatible struct{ Revision int }

func (e *Incompatible) Error() string {
	return fmt.Sprintf("pool agent create revision %d is incompatible with %d", e.Revision, Revision)
}

// Serve publishes discovery and serves only this process's create handler on
// the stream. The listener never accepts a replacement connection.
func Serve(w http.ResponseWriter, r *http.Request, handler http.Handler) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer func() { _ = ws.CloseNow() }()
	if ws.Subprotocol() != Subprotocol {
		return
	}
	data, err := json.Marshal(Discovery{CreateRevision: Revision})
	if err != nil {
		return
	}
	if err = ws.Write(r.Context(), websocket.MessageText, data); err != nil {
		return
	}
	wire := websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
	// net/http aborts its speculative next-request read with an expired
	// deadline. WebSocket NetConn closes the whole stream on that deadline;
	// net.Pipe supplies ordinary, resettable net.Conn deadline semantics.
	conn, bridge := net.Pipe()
	defer func() { _ = conn.Close(); _ = bridge.Close() }()
	copyStream := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		_ = conn.Close()
		_ = bridge.Close()
		_ = ws.CloseNow()
	}
	go copyStream(bridge, wire)
	go copyStream(wire, bridge)
	listener := &singleListener{conn: conn, done: make(chan struct{})}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	stop := context.AfterFunc(r.Context(), func() { _ = server.Close() })
	defer stop()
	_ = server.Serve(listener)
}

type singleListener struct {
	conn   net.Conn
	once   sync.Once
	done   chan struct{}
	closed sync.Once
}

func (l *singleListener) Accept() (net.Conn, error) {
	var conn net.Conn
	l.once.Do(func() { conn = &closingConn{Conn: l.conn, closeListener: l.Close} })
	if conn != nil {
		return conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *singleListener) Close() error   { l.closed.Do(func() { close(l.done) }); return nil }
func (l *singleListener) Addr() net.Addr { return l.conn.LocalAddr() }

type closingConn struct {
	net.Conn
	closeListener func() error
}

func (c *closingConn) Close() error { _ = c.closeListener(); return c.Conn.Close() }

// ConnectionLost is discovery that the agent never answered. It is distinct
// from an envelope the peer answered with a malformed revision.
type ConnectionLost struct{ Err error }

func (e *ConnectionLost) Error() string {
	return "pool create discovery connection lost: " + e.Err.Error()
}
func (e *ConnectionLost) Unwrap() error { return e.Err }
