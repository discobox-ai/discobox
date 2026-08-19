//go:build iroh

package endpoint

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The whole point of the transport is that the ordinary control-plane handler
// serves it unchanged, so the test drives a real http.Server over real iroh
// streams rather than a stand-in.
func TestIrohServesHTTPAndWebSockets(t *testing.T) {
	server, client := irohPair(t, func(IrohID) bool { return true })

	listener, display, cleanup, err := server.Listen()
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(cleanup)

	serverID, err := server.ID()
	if err != nil {
		t.Fatalf("ID() error = %v", err)
	}
	// The display value is what an operator copies, so it must be dialable on
	// its own: the ID plus the addresses that reach it.
	if !strings.HasPrefix(display, IrohURL(serverID)) {
		t.Fatalf("display = %q, want it to start with %q", display, IrohURL(serverID))
	}
	advertised, err := Parse(display)
	if err != nil {
		t.Fatalf("Parse(display) error = %v", err)
	}
	if len(advertised.IrohAddrs) == 0 {
		t.Fatal("display carries no direct addresses, so nothing can dial it without discovery")
	}

	var seenPeer IrohID
	mux := http.NewServeMux()
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	})
	mux.HandleFunc("/attach", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		typ, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		_ = conn.Write(r.Context(), typ, append([]byte("echo:"), data...))
	})

	httpServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			// This is the seam the authenticator will read: the peer's
			// verified identity, carried from the connection into the request.
			if ider, ok := c.(interface{ RemoteID() IrohID }); ok {
				seenPeer = ider.RemoteID()
			}
			return ctx
		},
	}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })

	httpClient := irohClient(t, client, serverID)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, LogicalHTTPBaseURL+"/whoami", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("GET /whoami: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s", resp.Status)
	}
	// HTTP/1.1 specifically: hijack is what attach depends on, and HTTP/2
	// would take it away.
	if got := string(body); got != "HTTP/1.1" {
		t.Fatalf("proto = %q, want HTTP/1.1", got)
	}

	clientID, err := client.ID()
	if err != nil {
		t.Fatalf("ID() error = %v", err)
	}
	if seenPeer != clientID {
		t.Fatalf("connection peer = %s, want %s", seenPeer, clientID)
	}

	// A websocket upgrade over the same transport: exec attach, the SSH
	// bridge, the TCP tunnel, and the project stream are all this.
	ws, wsResp, err := websocket.Dial(ctx, "ws://discobox.local/attach", &websocket.DialOptions{HTTPClient: httpClient})
	if wsResp != nil && wsResp.Body != nil {
		defer func() { _ = wsResp.Body.Close() }()
	}
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	if err := ws.Write(ctx, websocket.MessageBinary, []byte("hello")); err != nil {
		t.Fatalf("websocket write: %v", err)
	}
	_, msg, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("websocket read: %v", err)
	}
	if string(msg) != "echo:hello" {
		t.Fatalf("echo = %q, want %q", msg, "echo:hello")
	}
	_ = ws.Close(websocket.StatusNormalClosure, "done")
}

// An unenrolled peer is refused at accept, before any HTTP exists.
func TestIrohRefusesUnauthorizedEndpoint(t *testing.T) {
	server, client := irohPair(t, func(IrohID) bool { return false })

	listener, _, cleanup, err := server.Listen()
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(cleanup)

	reached := make(chan struct{}, 1)
	httpServer := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached <- struct{}{}
			w.WriteHeader(http.StatusOK)
		}),
	}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })

	serverID, err := server.ID()
	if err != nil {
		t.Fatalf("ID() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, LogicalHTTPBaseURL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := irohClient(t, client, serverID).Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("request succeeded from an unauthorized endpoint")
	}
	// The refusal must say why. The FFI reports every failure as an opaque
	// "IrohError", so without the peer's close reason a rejected operator has
	// nothing to act on.
	if !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("error = %v, want it to name authorization", err)
	}
	select {
	case <-reached:
		t.Fatal("an unauthorized peer reached the HTTP handler")
	default:
	}
}

// A deadline set while a Read is already blocked must interrupt it.
// http.Server's Hijack depends on exactly this: it aborts its pending
// background read by setting a deadline in the past and waiting for that read
// to return, so a conn that only honors deadlines captured at entry deadlocks
// every websocket upgrade.
func TestIrohConnDeadlineInterruptsBlockedRead(t *testing.T) {
	server, client := irohPair(t, func(IrohID) bool { return true })
	listener, _, cleanup, err := server.Listen()
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(cleanup)

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	serverID, err := server.ID()
	if err != nil {
		t.Fatalf("ID() error = %v", err)
	}
	addrs, err := server.DirectAddrs()
	if err != nil {
		t.Fatalf("DirectAddrs() error = %v", err)
	}
	transport, err := client.RoundTripper(serverID, nil, addrs...)
	if err != nil {
		t.Fatalf("RoundTripper() error = %v", err)
	}
	dialer, ok := transport.(localRoundTripper).base.(*http.Transport)
	if !ok {
		t.Fatal("transport is not an *http.Transport")
	}
	conn, err := dialer.DialContext(t.Context(), "tcp", "discobox.local:80")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	// A stream exists on the peer only once bytes flow on it.
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}

	var serverConn net.Conn
	select {
	case serverConn = <-accepted:
	case <-time.After(30 * time.Second):
		t.Fatal("listener did not accept the stream")
	}
	defer func() { _ = serverConn.Close() }()

	readErr := make(chan error, 1)
	go func() {
		// Drain everything the peer sent, so the next read has nothing
		// buffered to satisfy it and genuinely blocks.
		buf := make([]byte, len("hello"))
		if _, err := io.ReadFull(serverConn, buf); err != nil {
			readErr <- err
			return
		}
		_, err := serverConn.Read(buf)
		readErr <- err
	}()

	// Let the read block, then move the deadline into the past under it.
	time.Sleep(200 * time.Millisecond)
	if err := serverConn.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	select {
	case err := <-readErr:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("blocked Read returned %v, want os.ErrDeadlineExceeded", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a deadline set during a blocked Read did not interrupt it")
	}
}

// irohPair builds a server and a client endpoint that can find each other on
// this host without relays or discovery.
func irohPair(t *testing.T, authorize func(IrohID) bool) (server, client *IrohEndpoint) {
	t.Helper()
	server = newIrohEndpointForTest(t, IrohConfig{
		SecretKey:    newSecretKey(t),
		Authorize:    authorize,
		DisableRelay: true,
	})
	// Bind the server first so its socket addresses exist to hand the client.
	addrs, err := server.DirectAddrs()
	if err != nil {
		t.Fatalf("DirectAddrs() error = %v", err)
	}
	client = newIrohEndpointForTest(t, IrohConfig{
		SecretKey:    newSecretKey(t),
		DisableRelay: true,
		Locate:       func(IrohID) []string { return addrs },
	})
	return server, client
}

func newIrohEndpointForTest(t *testing.T, cfg IrohConfig) *IrohEndpoint {
	t.Helper()
	created, err := NewIrohEndpoint(cfg)
	if err != nil {
		t.Fatalf("NewIrohEndpoint() error = %v", err)
	}
	return created
}

func irohClient(t *testing.T, from *IrohEndpoint, to IrohID) *http.Client {
	t.Helper()
	transport, err := from.RoundTripper(to, nil)
	if err != nil {
		t.Fatalf("RoundTripper() error = %v", err)
	}
	return &http.Client{Transport: transport}
}

func newSecretKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return priv
}
