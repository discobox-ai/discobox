//go:build iroh

package endpoint

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	iroh "git.coopcloud.tech/decentral1se/iroh-go"
)

// irohALPN is the application protocol this transport negotiates. It names the
// protocol carried on the stream, which is ordinary HTTP/1.1, so a future
// protocol on the same endpoint gets its own ALPN rather than having to be
// distinguished inside the byte stream.
const irohALPN = "discobox/http/1"

// errIrohNotConfigured is returned when this build is asked to act on an iroh
// endpoint before [ConfigureIroh] has installed an identity.
var errIrohNotConfigured = errors.New("iroh endpoint used before ConfigureIroh")

// init installs the real transport over the refusing defaults in
// iroh_default.go. This is the only thing the build tag decides.
func init() {
	configureIroh = configureIrohFFI
	localIrohID = localIrohIDFFI
	irohRoundTripper = irohRoundTripperFFI
	irohListen = irohListenFFI
}

// IrohEndpoint is a bound iroh endpoint: one identity, one UDP socket, one
// admission policy. A process normally has exactly one — two would mean two
// addresses for one machine — so [ConfigureIroh] installs a default that
// [Listen] and [HTTPClient] use. It is a type rather than only a global so
// that a test, or anything else needing two peers at once, can hold both.
type IrohEndpoint struct {
	cfg IrohConfig

	mu       sync.Mutex
	endpoint *iroh.Endpoint
}

// NewIrohEndpoint prepares an endpoint. Binding is deferred to first use, so
// constructing one opens no socket.
func NewIrohEndpoint(cfg IrohConfig) (*IrohEndpoint, error) {
	if len(cfg.SecretKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("iroh secret key is %d bytes, want %d", len(cfg.SecretKey), ed25519.PrivateKeySize)
	}
	return &IrohEndpoint{cfg: cfg}, nil
}

// ID is the endpoint ID peers dial to reach this endpoint.
func (e *IrohEndpoint) ID() (IrohID, error) {
	pub, ok := e.cfg.SecretKey.Public().(ed25519.PublicKey)
	if !ok {
		return IrohID{}, errors.New("iroh secret key is not ed25519")
	}
	return IrohIDFromPublicKey(pub)
}

// bind opens the UDP socket on first use and returns the same endpoint after.
func (e *IrohEndpoint) bind() (*iroh.Endpoint, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.endpoint != nil {
		return e.endpoint, nil
	}
	builder := iroh.NewEndpointBuilder()
	if e.cfg.DisableRelay {
		builder.ApplyN0DisableRelay()
	} else {
		builder.ApplyN0()
	}
	// iroh takes the 32-byte seed, which is the first half of a Go ed25519
	// private key; the second half is the public key it derives anyway.
	if err := builder.SecretKey(e.cfg.SecretKey.Seed()); err != nil {
		return nil, fmt.Errorf("iroh secret key: %w", err)
	}
	builder.Alpns([][]byte{[]byte(irohALPN)})
	bound, err := builder.Bind()
	if err != nil {
		return nil, fmt.Errorf("bind iroh endpoint: %w", err)
	}
	e.endpoint = bound
	return e.endpoint, nil
}

// DirectAddrs are the socket addresses this endpoint is reachable at, for a
// caller that must locate a peer without discovery.
func (e *IrohEndpoint) DirectAddrs() ([]string, error) {
	bound, err := e.bind()
	if err != nil {
		return nil, err
	}
	return dialableAddrs(bound), nil
}

// dialableAddrs are the endpoint's addresses with the unspecified ones
// rewritten to loopback.
//
// BoundSockets reports the bind address, which for the default bind is
// 0.0.0.0 and [::] — correct as a bind, useless as a dial target. The endpoint's
// own EndpointAddr carries the concrete addresses discovery would publish; the
// wildcard entries are kept as loopback so a peer on this machine can still
// reach it, which is the `task dev` case.
func dialableAddrs(ep *iroh.Endpoint) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(addr string) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
			if ip.To4() != nil {
				addr = net.JoinHostPort("127.0.0.1", port)
			} else {
				addr = net.JoinHostPort("::1", port)
			}
		}
		if _, ok := seen[addr]; ok {
			return
		}
		seen[addr] = struct{}{}
		out = append(out, addr)
	}
	if addr := ep.Addr(); addr != nil {
		for _, direct := range addr.DirectAddresses() {
			add(direct)
		}
	}
	for _, bound := range ep.BoundSockets() {
		add(bound)
	}
	return out
}

// The process's default endpoint, installed by ConfigureIroh. Listen and
// HTTPClient resolve an iroh:// endpoint through it, so the schemes that have
// no identity to configure keep their existing call signatures.
var (
	defaultIrohMu sync.Mutex
	defaultIroh   *IrohEndpoint
)

func configureIrohFFI(cfg IrohConfig) error {
	created, err := NewIrohEndpoint(cfg)
	if err != nil {
		return err
	}
	defaultIrohMu.Lock()
	defer defaultIrohMu.Unlock()
	if defaultIroh != nil {
		return errors.New("iroh is already configured for this process")
	}
	defaultIroh = created
	return nil
}

func localIrohIDFFI() (IrohID, error) {
	configured, err := defaultIrohEndpoint()
	if err != nil {
		return IrohID{}, err
	}
	return configured.ID()
}

func defaultIrohEndpoint() (*IrohEndpoint, error) {
	defaultIrohMu.Lock()
	defer defaultIrohMu.Unlock()
	if defaultIroh == nil {
		return nil, errIrohNotConfigured
	}
	return defaultIroh, nil
}

func irohIDOf(id *iroh.EndpointId) (IrohID, error) {
	if id == nil {
		return IrohID{}, errors.New("iroh peer has no endpoint ID")
	}
	var out IrohID
	raw := id.ToBytes()
	if len(raw) != IrohIDSize {
		return IrohID{}, fmt.Errorf("iroh endpoint ID is %d bytes, want %d", len(raw), IrohIDSize)
	}
	copy(out[:], raw)
	return out, nil
}

// irohRoundTripper serves HTTP over streams opened to one peer. Each request
// gets its own bidirectional QUIC stream, which is what the unix transport gets
// from a socket connection and what keeps hijack — and therefore websockets —
// working.
func irohRoundTripperFFI(target Endpoint, base http.RoundTripper) (http.RoundTripper, error) {
	configured, err := defaultIrohEndpoint()
	if err != nil {
		return nil, err
	}
	id, err := target.IrohID()
	if err != nil {
		return nil, err
	}
	return configured.RoundTripper(id, base, target.IrohAddrs...)
}

// RoundTripper dials one peer and serves HTTP over streams opened to it. Any
// direct addresses given are tried alongside whatever discovery finds, which is
// what makes a peer reachable before a discovery service exists.
func (e *IrohEndpoint) RoundTripper(id IrohID, base http.RoundTripper, direct ...string) (http.RoundTripper, error) {
	ep, err := e.bind()
	if err != nil {
		return nil, err
	}
	peer, err := iroh.EndpointIdFromBytes(id[:])
	if err != nil {
		return nil, fmt.Errorf("iroh endpoint ID: %w", err)
	}

	var (
		mu   sync.Mutex
		conn *iroh.Connection
	)
	// One QUIC connection per peer, redialed if it dies. Streams are cheap;
	// connections carry the handshake, the path discovery, and the NAT
	// traversal state, so tearing one down per request would pay all of that
	// again on every call.
	dial := func() (*iroh.Connection, error) {
		mu.Lock()
		defer mu.Unlock()
		if conn != nil && !connClosed(conn) {
			return conn, nil
		}
		addrs := direct
		if e.cfg.Locate != nil {
			addrs = append(addrs, e.cfg.Locate(id)...)
		}
		addr := iroh.NewEndpointAddr(peer, nil, addrs)
		opened, err := ep.Connect(addr, []byte(irohALPN))
		if err != nil {
			// iroh.As unwraps the FFI's opaque IrohError into the message and
			// debug text underneath it. Without it every failure here — a
			// refusal, an unreachable peer, a relay problem — reads as the
			// single word "IrohError".
			return nil, fmt.Errorf("dial iroh endpoint %s: %w", id.Short(), irohErr(err))
		}
		conn = opened
		return conn, nil
	}

	transport := cloneTransport(base)
	transport.DialContext = func(_ context.Context, _, _ string) (net.Conn, error) {
		active, err := dial()
		if err != nil {
			return nil, err
		}
		stream, err := active.OpenBi()
		if err != nil {
			// The peer's close reason is the only place a refusal explains
			// itself: the FFI reports the failure as an opaque error, so
			// without this a rejected client sees nothing but "IrohError".
			if reason := active.CloseReason(); reason != nil && *reason != "" {
				return nil, fmt.Errorf("iroh endpoint %s refused the connection: %s", id.Short(), *reason)
			}
			return nil, fmt.Errorf("open iroh stream: %w", irohErr(err))
		}
		return newIrohConn(active, stream, id), nil
	}
	// HTTP/2 cannot be negotiated here: there is no TLS handshake of our own to
	// carry ALPN, and the stream is already the connection.
	transport.ForceAttemptHTTP2 = false
	transport.TLSClientConfig = nil
	// No proxy. DialContext reaches one endpoint ID and ignores the address it
	// is handed, so an HTTP proxy in the environment cannot be honored and
	// must not be attempted: doing so turns every request into a CONNECT to a
	// peer that is not a proxy.
	transport.Proxy = nil
	return localRoundTripper{base: transport}, nil
}

// irohErr replaces the FFI's opaque error with the message and debug text
// underneath it. Without this every failure — a refusal, an unreachable peer, a
// relay problem — reads as the single word "IrohError".
//
// iroh-go ships an As() helper meant for exactly this, but it matches the value
// type while the bindings return *IrohError, so it never fires.
func irohErr(err error) error {
	var ffiErr *iroh.IrohError
	if errors.As(err, &ffiErr) {
		if debug := ffiErr.DebugMessage(); debug != "" && debug != ffiErr.Message() {
			return fmt.Errorf("%s (%s)", ffiErr.Message(), debug)
		}
		return errors.New(ffiErr.Message())
	}
	return err
}

func connClosed(conn *iroh.Connection) bool {
	return conn.CloseReason() != nil
}

// irohListen binds this machine's endpoint and presents accepted streams as a
// net.Listener, so the ordinary control-plane handler serves them unchanged.
func irohListenFFI(Endpoint) (net.Listener, string, func(), error) {
	configured, err := defaultIrohEndpoint()
	if err != nil {
		return nil, "", nil, err
	}
	return configured.Listen()
}

// Listen accepts streams on this endpoint as a net.Listener.
func (e *IrohEndpoint) Listen() (net.Listener, string, func(), error) {
	ep, err := e.bind()
	if err != nil {
		return nil, "", nil, err
	}
	id, err := irohIDOf(ep.Id())
	if err != nil {
		return nil, "", nil, err
	}
	listener := &irohListener{
		endpoint:  ep,
		authorize: e.cfg.Authorize,
		streams:   make(chan net.Conn),
		done:      make(chan struct{}),
	}
	go listener.accept()
	// The dialable URL is only known once the endpoint is bound, which is why
	// Listen reports the address it ended up with rather than echoing the one
	// it was given. The direct addresses ride along so the logged URL works
	// before any discovery service does.
	return listener, IrohURLWithAddrs(id, dialableAddrs(ep)), func() { _ = listener.Close() }, nil
}

type irohListener struct {
	endpoint  *iroh.Endpoint
	authorize func(IrohID) bool
	streams   chan net.Conn
	done      chan struct{}

	closeOnce sync.Once
	errMu     sync.Mutex
	err       error
}

func (l *irohListener) accept() {
	for {
		incoming := l.endpoint.AcceptNext()
		if incoming == nil || *incoming == nil {
			l.setErr(net.ErrClosed)
			return
		}
		select {
		case <-l.done:
			_ = (*incoming).Ignore()
			return
		default:
		}
		go l.admit(*incoming)
	}
}

// admit completes one connection's handshake and decides whether it may speak
// to the control plane at all. An unenrolled peer is closed here, before any
// HTTP exists, with the reason carried in the QUIC close so the caller reads
// why rather than seeing a bare disconnect.
func (l *irohListener) admit(incoming *iroh.Incoming) {
	accepting, err := incoming.Accept()
	if err != nil {
		return
	}
	conn, err := accepting.Connect()
	if err != nil {
		return
	}
	peer, err := irohIDOf(conn.RemoteId())
	if err != nil {
		_ = conn.Close(1, []byte("endpoint identity is unreadable"))
		return
	}
	if l.authorize == nil || !l.authorize(peer) {
		_ = conn.Close(1, []byte("endpoint "+peer.String()+" is not authorized on this server"))
		return
	}
	for {
		stream, err := conn.AcceptBi()
		if err != nil {
			return
		}
		select {
		case l.streams <- newIrohConn(conn, stream, peer):
		case <-l.done:
			return
		}
	}
}

func (l *irohListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.streams:
		return conn, nil
	case <-l.done:
		return nil, l.acceptErr()
	}
}

func (l *irohListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.done)
		_ = l.endpoint.Close()
	})
	return nil
}

func (l *irohListener) Addr() net.Addr {
	id, err := irohIDOf(l.endpoint.Id())
	if err != nil {
		return irohAddr{}
	}
	return irohAddr{id: id}
}

func (l *irohListener) setErr(err error) {
	l.errMu.Lock()
	if l.err == nil {
		l.err = err
	}
	l.errMu.Unlock()
	l.Close()
}

func (l *irohListener) acceptErr() error {
	l.errMu.Lock()
	defer l.errMu.Unlock()
	if l.err != nil {
		return l.err
	}
	return net.ErrClosed
}

type irohAddr struct {
	id IrohID
}

func (irohAddr) Network() string { return "iroh" }

func (a irohAddr) String() string {
	if a.id.IsZero() {
		return "iroh://"
	}
	return IrohURL(a.id)
}

// irohConn adapts one bidirectional stream to net.Conn.
//
// The FFI exposes reads as RecvStream.Read(sizeLimit) with no context,
// deadline, or cancellation, so a read in flight cannot be abandoned without
// losing the bytes it consumed. A single pump goroutine owns the FFI read loop
// and hands chunks over a channel: a deadline then expires on the channel
// receive rather than on the FFI call, the pending bytes stay queued for the
// next Read, and SetReadDeadline means what net.Conn says it means. The pump
// reads at most one chunk ahead, so this buys correct deadlines rather than
// unbounded buffering.
type irohConn struct {
	stream *iroh.BiStream
	recv   *iroh.RecvStream
	send   *iroh.SendStream
	// conn is the connection the stream belongs to, kept so that a read
	// failure can report the peer's close reason. The FFI surfaces every
	// failure as an opaque IrohError, so without this a refusal is
	// indistinguishable from any other network fault.
	conn *iroh.Connection
	peer IrohID

	chunks  chan []byte
	pumpErr chan error
	closed  chan struct{}

	closeOnce sync.Once

	mu      sync.Mutex
	pending []byte
	// A deadline must interrupt a call that is *already* blocked, not just
	// bound the next one: http.Server's Hijack aborts its pending background
	// read by setting a deadline in the past and waiting for that read to
	// return. Each wake channel is closed and replaced when its deadline
	// changes, so a blocked Read or Write re-evaluates instead of waiting on
	// the deadline it captured on entry. Without this, every websocket upgrade
	// deadlocks in Hijack.
	readDead  time.Time
	readWake  chan struct{}
	writeDead time.Time
	writeWake chan struct{}
}

const irohReadChunk = 64 * 1024

// explain replaces an opaque stream error with the peer's close reason when
// there is one, so a refused client is told why instead of being left to guess.
func (c *irohConn) explain(err error) error {
	if c.conn == nil {
		return irohErr(err)
	}
	if reason := c.conn.CloseReason(); reason != nil && *reason != "" {
		return fmt.Errorf("iroh endpoint %s closed the connection: %s", c.peer.Short(), *reason)
	}
	return irohErr(err)
}

func newIrohConn(conn *iroh.Connection, stream *iroh.BiStream, peer IrohID) *irohConn {
	c := &irohConn{
		stream:    stream,
		recv:      stream.Recv(),
		send:      stream.Send(),
		conn:      conn,
		peer:      peer,
		chunks:    make(chan []byte),
		pumpErr:   make(chan error, 1),
		closed:    make(chan struct{}),
		readWake:  make(chan struct{}),
		writeWake: make(chan struct{}),
	}
	go c.pump()
	return c
}

func (c *irohConn) pump() {
	for {
		data, err := c.recv.Read(irohReadChunk)
		if err != nil {
			c.pumpErr <- c.explain(err)
			return
		}
		if len(data) == 0 {
			c.pumpErr <- io.EOF
			return
		}
		select {
		case c.chunks <- data:
		case <-c.closed:
			return
		}
	}
}

// RemoteID is the authenticated identity of the peer on the other end. The
// listener has already decided this peer may connect; carrying the ID here is
// what lets the HTTP layer build a principal from it.
func (c *irohConn) RemoteID() IrohID { return c.peer }

func (c *irohConn) Read(p []byte) (int, error) {
	for {
		c.mu.Lock()
		if len(c.pending) > 0 {
			n := copy(p, c.pending)
			c.pending = c.pending[n:]
			c.mu.Unlock()
			return n, nil
		}
		deadline, wake := c.readDead, c.readWake
		c.mu.Unlock()

		var timeout <-chan time.Time
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			timer := time.NewTimer(remaining)
			defer timer.Stop()
			timeout = timer.C
		}

		select {
		case data := <-c.chunks:
			n := copy(p, data)
			if n < len(data) {
				c.mu.Lock()
				c.pending = data[n:]
				c.mu.Unlock()
			}
			return n, nil
		case err := <-c.pumpErr:
			// Put it back so every later Read reports the same end, rather than
			// the first one consuming it and the rest blocking forever.
			c.pumpErr <- err
			return 0, err
		case <-timeout:
			return 0, os.ErrDeadlineExceeded
		case <-wake:
			// The deadline changed under us; re-read it and wait again.
		case <-c.closed:
			return 0, net.ErrClosed
		}
	}
}

func (c *irohConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	done := make(chan error, 1)
	go func() { done <- c.send.WriteAll(p) }()

	for {
		c.mu.Lock()
		deadline, wake := c.writeDead, c.writeWake
		c.mu.Unlock()

		var timeout <-chan time.Time
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				c.failWrite()
				return 0, os.ErrDeadlineExceeded
			}
			timer := time.NewTimer(remaining)
			defer timer.Stop()
			timeout = timer.C
		}

		select {
		case err := <-done:
			if err != nil {
				return 0, irohErr(err)
			}
			return len(p), nil
		case <-timeout:
			c.failWrite()
			return 0, os.ErrDeadlineExceeded
		case <-wake:
			// The deadline changed under us; re-read it and wait again.
		case <-c.closed:
			return 0, net.ErrClosed
		}
	}
}

// failWrite abandons a write that timed out. Unlike a read, it cannot be
// resumed: the peer may have received any prefix of it, so the stream's framing
// is no longer known. Resetting says so rather than leaving a half-written
// message behind.
func (c *irohConn) failWrite() {
	_ = c.send.Reset(1)
}

// CloseWrite is how a caller says "done sending" without tearing down the
// stream, which is what the exec attach and TCP tunnel protocols need and what
// a websocket cannot express (ADR 0024 §4).
func (c *irohConn) CloseWrite() error {
	return c.send.Finish()
}

func (c *irohConn) Close() error {
	c.closeOnce.Do(func() {
		// Closing the channel is what unblocks every waiting Read and Write,
		// and it happens synchronously so Close means closed to our callers.
		close(c.closed)
		// The FFI teardown does not: Stop waits behind the pump's in-flight
		// Read, and that Read only ends when the stream does — so calling it
		// here would deadlock every `defer conn.Close()`. It runs behind
		// Close instead, which still sends the FIN and reset, just without
		// making the caller wait for the peer.
		go func() {
			_ = c.recv.Stop(0)
			_ = c.send.Finish()
		}()
	})
	return nil
}

func (c *irohConn) LocalAddr() net.Addr  { return irohAddr{} }
func (c *irohConn) RemoteAddr() net.Addr { return irohAddr{id: c.peer} }

func (c *irohConn) SetDeadline(t time.Time) error {
	_ = c.SetReadDeadline(t)
	_ = c.SetWriteDeadline(t)
	return nil
}

func (c *irohConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDead = t
	close(c.readWake)
	c.readWake = make(chan struct{})
	return nil
}

func (c *irohConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDead = t
	close(c.writeWake)
	c.writeWake = make(chan struct{})
	return nil
}
