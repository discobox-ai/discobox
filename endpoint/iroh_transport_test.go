package endpoint

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The whole point of the transport is that the ordinary control-plane handler
// serves it unchanged, so the test drives a real http.Server over real iroh
// streams rather than a stand-in.
func TestIrohServesHTTPAndWebSockets(t *testing.T) {
	server, client := irohPair(t, admitAll)

	listener, display, cleanup, err := server.Listen()
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(cleanup)

	serverID, err := server.ID()
	if err != nil {
		t.Fatalf("ID() error = %v", err)
	}
	// The display value is what an operator copies and what a server logs, so
	// it is the address and nothing else: no query string, and none of this
	// host's socket addresses (ADR 0097 §3).
	if display != IrohURL(serverID) {
		t.Fatalf("display = %q, want exactly %q", display, IrohURL(serverID))
	}
	advertised, err := Parse(display)
	if err != nil {
		t.Fatalf("Parse(display) error = %v", err)
	}
	if len(advertised.IrohAddrs) != 0 {
		t.Fatalf("display advertises %v; those are this host's own addresses", advertised.IrohAddrs)
	}

	// The fallback is a separate value, offered beside the address for a peer
	// that cannot resolve it through discovery (ADR 0097 §3). It has to carry
	// the same peer and at least one address, or there is no way into a
	// deployment that has no discovery.
	fallback, err := server.fallbackURL()
	if err != nil {
		t.Fatalf("fallbackURL() error = %v", err)
	}
	withAddrs, err := Parse(fallback)
	if err != nil {
		t.Fatalf("Parse(fallback) error = %v", err)
	}
	fallbackID, err := withAddrs.IrohID()
	if err != nil {
		t.Fatalf("IrohID() error = %v", err)
	}
	if fallbackID != serverID {
		t.Fatalf("fallback names %s, want %s", fallbackID, serverID)
	}
	if len(withAddrs.IrohAddrs) == 0 {
		t.Fatal("the fallback carries no addresses, so it is not a way in without discovery")
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
			if peer, ok := IrohPeer(c); ok {
				seenPeer = peer
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
	server, client := irohPair(t, refuseAll)

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
	server, client := irohPair(t, admitAll)
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
func irohPair(t *testing.T, authorize func(context.Context, IrohID) error) (server, client *IrohEndpoint) {
	t.Helper()
	server = newIrohEndpointForTest(t, IrohConfig{
		SecretKey: newSecretKey(t),
		Authorize: authorize,
	})
	// Bind the server first so its socket addresses exist to hand the client.
	addrs, err := server.DirectAddrs()
	if err != nil {
		t.Fatalf("DirectAddrs() error = %v", err)
	}
	client = newIrohEndpointForTest(t, IrohConfig{
		SecretKey: newSecretKey(t),
		Locate:    func(IrohID) []string { return addrs },
	})
	return server, client
}

func newIrohEndpointForTest(t *testing.T, cfg IrohConfig) *IrohEndpoint {
	t.Helper()
	cfg.DisableRelay = true
	cfg.DisableDiscovery = true
	cfg.BindAddrs = loopbackBind()
	created, err := NewIrohEndpoint(cfg)
	if err != nil {
		t.Fatalf("NewIrohEndpoint() error = %v", err)
	}
	return created
}

// loopbackBind is what every endpoint in these tests binds. They pair two
// endpoints on this one machine and reach each other through Locate, so a
// wildcard socket buys them nothing — and costs a Windows Firewall prompt on
// every run, because `go test` builds to a fresh temporary path each time and
// an allowance is granted to a program path.
//
// The bind is half of it; newIrohEndpointForTest turns off relays and address
// lookup for the other half. Between them the suite reaches for nothing outside
// this host, which is the configuration iroh-go's own tests and its echo
// example use for the same reason.
func loopbackBind() []netip.AddrPort {
	return []netip.AddrPort{netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), 0)}
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

// admitAll and refuseAll are the two admission policies these tests need. They
// are named rather than written inline at each call because the signature
// carries a context the tests never use, and repeating that is noise.
func admitAll(context.Context, IrohID) error { return nil }

func refuseAll(_ context.Context, id IrohID) error {
	return fmt.Errorf("endpoint %s is not authorized on this server", id)
}

// A refusal carries the policy's own words, not this package's. "Not enrolled"
// and "the server is still starting" send an operator to different places, and
// the close reason is the only channel that distinction has (ADR 0095 §4).
func TestIrohRefusalReasonReachesPeer(t *testing.T) {
	const reason = "this server has not finished starting"
	server, client := irohPair(t, func(context.Context, IrohID) error {
		return errors.New(reason)
	})

	listener, _, cleanup, err := server.Listen()
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(cleanup)
	httpServer := &http.Server{ReadHeaderTimeout: 10 * time.Second}
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
		t.Fatal("request succeeded against a refusing endpoint")
	}
	if !strings.Contains(err.Error(), reason) {
		t.Fatalf("error = %v, want it to carry the policy's reason %q", err, reason)
	}
}

// Closing the endpoint releases an admission check that is still waiting.
//
// The control plane's gate waits for a database that startup has not opened
// yet (ADR 0095 §4). When that startup fails instead, the listener is torn
// down — and a waiter with no cancellation would sit there until the process
// died rather than being refused by it.
func TestIrohCloseReleasesWaitingAuthorize(t *testing.T) {
	entered := make(chan struct{}, 1)
	released := make(chan error, 1)
	server, client := irohPair(t, func(ctx context.Context, _ IrohID) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		released <- ctx.Err()
		return ctx.Err()
	})

	listener, _, cleanup, err := server.Listen()
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	httpServer := &http.Server{ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })

	serverID, err := server.ID()
	if err != nil {
		t.Fatalf("ID() error = %v", err)
	}
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, LogicalHTTPBaseURL+"/", nil)
		if err != nil {
			return
		}
		resp, err := irohClient(t, client, serverID).Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	select {
	case <-entered:
	case <-time.After(60 * time.Second):
		t.Fatal("the admission policy was never consulted")
	}

	cleanup()

	select {
	case err := <-released:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting policy released with %v, want context.Canceled", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("closing the endpoint did not release the waiting admission policy")
	}
}

// A closed endpoint stays closed.
//
// bind is lazy, so before this a second Listen would return a working listener
// whose admission checks all saw the context close had already canceled: every
// peer refused with "this server is shutting down", on a server that is up.
func TestIrohEndpointDoesNotComeBackAfterClose(t *testing.T) {
	server, _ := irohPair(t, admitAll)
	_, _, cleanup, err := server.Listen()
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	cleanup()

	if _, _, _, err := server.Listen(); !errors.Is(err, errIrohEndpointClosed) {
		t.Fatalf("Listen() after close = %v, want errIrohEndpointClosed", err)
	}
	if _, err := server.DirectAddrs(); !errors.Is(err, errIrohEndpointClosed) {
		t.Fatalf("DirectAddrs() after close = %v, want errIrohEndpointClosed", err)
	}
}

// The point of ADR 0104: a client dials with a key nobody has ever heard of and
// is admitted as the identity it enrolled, because it presents a certificate
// binding the two. The server's allowlist is unchanged — it still admits an
// identity, not an endpoint.
func TestIrohAdmitsAnEphemeralEndpointBearingACertificate(t *testing.T) {
	enrolled := newSecretKey(t)
	enrolledID, err := IrohIDFromPublicKey(enrolled.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatalf("IrohIDFromPublicKey() error = %v", err)
	}

	var admitted []IrohID
	server := newIrohEndpointForTest(t, IrohConfig{
		SecretKey: newSecretKey(t),
		Authorize: func(_ context.Context, id IrohID) error {
			admitted = append(admitted, id)
			if id != enrolledID {
				return fmt.Errorf("endpoint %s is not authorized on this server", id)
			}
			return nil
		},
	})
	addrs, err := server.DirectAddrs()
	if err != nil {
		t.Fatalf("DirectAddrs() error = %v", err)
	}

	// The transport key is generated for this client alone and never enrolled.
	ephemeral := newSecretKey(t)
	ephemeralID, err := IrohIDFromPublicKey(ephemeral.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatalf("IrohIDFromPublicKey() error = %v", err)
	}
	cert, err := SignPeerCert(enrolled, ephemeralID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SignPeerCert() error = %v", err)
	}
	client := newIrohEndpointForTest(t, IrohConfig{
		SecretKey:   ephemeral,
		Certificate: &cert,
		Locate:      func(IrohID) []string { return addrs },
	})

	listener, _, cleanup, err := server.Listen()
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(cleanup)
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "pong")
	})
	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })

	serverID, err := server.ID()
	if err != nil {
		t.Fatalf("ID() error = %v", err)
	}
	if got := irohPing(t, irohClient(t, client, serverID)); got != "pong" {
		t.Fatalf("/ping = %q, want %q", got, "pong")
	}

	// The allowlist saw the enrolled identity once, never the endpoint that
	// dialed. Evaluating the ephemeral ID first would refuse and log it as
	// unenrolled on the way to admitting it.
	if len(admitted) != 1 || admitted[0] != enrolledID {
		t.Fatalf("the policy was asked about %v, want exactly the enrolled identity %s", admitted, enrolledID)
	}
	if slices.Contains(admitted, ephemeralID) {
		t.Fatalf("the policy was asked about the ephemeral endpoint %s", ephemeralID)
	}
}

// A certificate is worthless to anyone but the endpoint it names, and the
// server is where that has to hold: presenting someone else's certificate from
// a different endpoint must be refused, however well signed it is.
func TestIrohRefusesACertificateIssuedForAnotherEndpoint(t *testing.T) {
	enrolled := newSecretKey(t)
	enrolledID, err := IrohIDFromPublicKey(enrolled.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatalf("IrohIDFromPublicKey() error = %v", err)
	}
	server := newIrohEndpointForTest(t, IrohConfig{
		SecretKey: newSecretKey(t),
		Authorize: func(_ context.Context, id IrohID) error {
			if id != enrolledID {
				return fmt.Errorf("endpoint %s is not authorized on this server", id)
			}
			return nil
		},
	})
	addrs, err := server.DirectAddrs()
	if err != nil {
		t.Fatalf("DirectAddrs() error = %v", err)
	}

	// A certificate for somebody else's endpoint, correctly signed by a
	// correctly enrolled identity. The thief holds neither private key it
	// names, and the handshake is what gives them away.
	victimPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	victim, err := IrohIDFromPublicKey(victimPub)
	if err != nil {
		t.Fatalf("IrohIDFromPublicKey() error = %v", err)
	}
	stolen, err := SignPeerCert(enrolled, victim, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SignPeerCert() error = %v", err)
	}
	thief := newIrohEndpointForTest(t, IrohConfig{
		SecretKey:   newSecretKey(t),
		Certificate: &stolen,
		Locate:      func(IrohID) []string { return addrs },
	})

	listener, _, cleanup, err := server.Listen()
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(cleanup)
	httpServer := &http.Server{ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })

	serverID, err := server.ID()
	if err != nil {
		t.Fatalf("ID() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, LogicalHTTPBaseURL+"/ping", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := irohClient(t, thief, serverID).Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a certificate issued for another endpoint was accepted")
	}
	if !strings.Contains(err.Error(), "arrived from") {
		t.Fatalf("error = %v, want the refusal to say the certificate was for another endpoint", err)
	}
}

// irohPing is one request, with a deadline short enough that a client that
// never gets in fails the test rather than hanging it.
func irohPing(t *testing.T, client *http.Client) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, LogicalHTTPBaseURL+"/ping", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /ping: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

// Locate is offered addresses and Reached hands them back, so a client that
// has connected once can dial the address next time instead of waiting on
// discovery and a relay to rebuild the path it already knows.
func TestIrohReportsTheAddressAPeerAnsweredOn(t *testing.T) {
	server, client := irohPair(t, admitAll)

	listener, _, cleanup, err := server.Listen()
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(cleanup)
	serverID, err := server.ID()
	if err != nil {
		t.Fatalf("ID() error = %v", err)
	}

	// Reached is told from a watch on the connection, not from the request, so
	// the test waits for it rather than reading it the moment the request ends.
	reachedCh := make(chan []string, 1)
	client.cfg.Reached = func(id IrohID, addrs []string) {
		if id != serverID {
			t.Errorf("Reached reported %s, want the peer that was dialed, %s", id, serverID)
		}
		select {
		case reachedCh <- append([]string(nil), addrs...):
		default:
		}
	}

	httpServer := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, LogicalHTTPBaseURL+"/", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	resp, err := irohClient(t, client, serverID).Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	_ = resp.Body.Close()

	var reached []string
	select {
	case reached = <-reachedCh:
	case <-ctx.Done():
		t.Fatal("Reached was told nothing, so a client that just connected has nothing to try next time")
	}
	// It has to be an address, not a relay URL: the whole point is that it can
	// be dialed without one.
	for _, addr := range reached {
		if _, err := netip.ParseAddrPort(addr); err != nil {
			t.Fatalf("Reached reported %q, which is not a socket address: %v", addr, err)
		}
	}
	// And it has to be an address this server is actually on, or it is a hint
	// that sends the next dial somewhere else entirely.
	bound, err := server.DirectAddrs()
	if err != nil {
		t.Fatalf("DirectAddrs() error = %v", err)
	}
	if !slices.Contains(bound, reached[0]) {
		t.Fatalf("Reached reported %q, which is not one of the server's addresses %v", reached[0], bound)
	}
}

// A nil Reached is the ordinary case for anything that dials a peer once, and
// it must not cost the caller a request.
func TestIrohWithoutReachedServesNormally(t *testing.T) {
	server, client := irohPair(t, admitAll)
	if client.cfg.Reached != nil {
		t.Fatal("the test pair should not set Reached")
	}

	listener, _, cleanup, err := server.Listen()
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(cleanup)
	serverID, err := server.ID()
	if err != nil {
		t.Fatalf("ID() error = %v", err)
	}

	httpServer := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, LogicalHTTPBaseURL+"/", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	resp, err := irohClient(t, client, serverID).Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// A server offered the sockets of its last start comes back on them, so the
// port every client remembered is still the port it answers on.
func TestIrohBindsThePreferredSockets(t *testing.T) {
	first := newIrohEndpointForTest(t, IrohConfig{SecretKey: newSecretKey(t)})
	var remembered []netip.AddrPort
	first.cfg.Bound = func(sockets []netip.AddrPort) { remembered = sockets }
	if _, err := first.bind(); err != nil {
		t.Fatalf("bind() error = %v", err)
	}
	first.close()
	if len(remembered) == 0 || remembered[0].Port() == 0 {
		t.Fatalf("Bound reported %v, want the socket that was bound", remembered)
	}

	again := newIrohEndpointForTest(t, IrohConfig{SecretKey: newSecretKey(t), PreferredBindAddrs: remembered})
	t.Cleanup(again.close)
	var bound []netip.AddrPort
	again.cfg.Bound = func(sockets []netip.AddrPort) { bound = sockets }
	if _, err := again.bind(); err != nil {
		t.Fatalf("bind() error = %v", err)
	}
	if !slices.Equal(bound, remembered) {
		t.Fatalf("bound %v, want the preferred %v", bound, remembered)
	}
}

// A port taken while the server was down is somebody else's now. That costs
// the server its old port, never its listener.
func TestIrohBindsFreshSocketsWhenThePreferredOnesAreTaken(t *testing.T) {
	taken, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP() error = %v", err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	held := taken.LocalAddr().(*net.UDPAddr).AddrPort()

	ep := newIrohEndpointForTest(t, IrohConfig{SecretKey: newSecretKey(t), PreferredBindAddrs: []netip.AddrPort{held}})
	t.Cleanup(ep.close)
	var bound []netip.AddrPort
	ep.cfg.Bound = func(sockets []netip.AddrPort) { bound = sockets }
	if _, err := ep.bind(); err != nil {
		t.Fatalf("bind() error = %v, want a fresh socket in place of the taken one", err)
	}
	if len(bound) == 0 {
		t.Fatal("Bound reported nothing, so the next start has nothing to prefer")
	}
	if slices.Contains(bound, held) {
		t.Fatalf("bound %v, which includes %v that another socket holds", bound, held)
	}
}

// Offered sockets replace iroh's default set rather than adjusting it, so a
// family missing from them must still be bound: a start that recorded IPv4
// alone, because IPv6 failed that once, must not pin the endpoint to IPv4.
func TestIrohPreferredSocketsDoNotDropAFamily(t *testing.T) {
	probe, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Skipf("this host cannot bind IPv6 loopback: %v", err)
	}
	_ = probe.Close()
	free, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP() error = %v", err)
	}
	remembered := free.LocalAddr().(*net.UDPAddr).AddrPort()
	_ = free.Close()

	ep := newIrohEndpointForTest(t, IrohConfig{SecretKey: newSecretKey(t), PreferredBindAddrs: []netip.AddrPort{remembered}})
	ep.cfg.BindAddrs = append(loopbackBind(), netip.AddrPortFrom(netip.IPv6Loopback(), 0))
	t.Cleanup(ep.close)
	var bound []netip.AddrPort
	ep.cfg.Bound = func(sockets []netip.AddrPort) { bound = sockets }
	if _, err := ep.bind(); err != nil {
		t.Fatalf("bind() error = %v", err)
	}
	if !slices.Contains(bound, remembered) {
		t.Fatalf("bound %v, want the remembered %v among them", bound, remembered)
	}
	if !slices.ContainsFunc(bound, func(s netip.AddrPort) bool { return s.Addr().Is6() }) {
		t.Fatalf("bound %v, which has no IPv6 socket although the ordinary bind asks for one", bound)
	}
}

func TestPreferredBinds(t *testing.T) {
	v4 := netip.MustParseAddrPort("0.0.0.0:46966")
	v6 := netip.MustParseAddrPort("[::]:54867")
	for name, tc := range map[string]struct {
		cfg  IrohConfig
		want [][]netip.AddrPort
	}{
		"nothing preferred": {cfg: IrohConfig{}, want: nil},
		"both families remembered": {
			cfg:  IrohConfig{PreferredBindAddrs: []netip.AddrPort{v4, v6}},
			want: [][]netip.AddrPort{{v4, v6}},
		},
		// The first attempt puts IPv6 back on a fresh port; the second is the
		// remembered set, for a host that still has no IPv6.
		"IPv6 missing": {
			cfg:  IrohConfig{PreferredBindAddrs: []netip.AddrPort{v4}},
			want: [][]netip.AddrPort{{v4, netip.AddrPortFrom(netip.IPv6Unspecified(), 0)}, {v4}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := preferredBinds(tc.cfg)
			if !slices.EqualFunc(got, tc.want, slices.Equal) {
				t.Fatalf("preferredBinds() = %v, want %v", got, tc.want)
			}
		})
	}
}
