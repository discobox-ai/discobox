package endpoint

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	iroh "github.com/discobox-ai/iroh-go"
)

// irohALPN is the application protocol this transport negotiates. It names the
// protocol carried on the stream, which is ordinary HTTP/1.1, so a future
// protocol on the same endpoint gets its own ALPN rather than having to be
// distinguished inside the byte stream.
const irohALPN = "discobox/http/1"

// irohTeardownTimeout bounds closing the bound endpoint, so a peer that has
// stopped answering cannot hold up a shutdown.
const irohTeardownTimeout = 5 * time.Second

// errIrohNotConfigured is returned when an iroh endpoint is acted on before
// [ConfigureIroh] has installed an identity.
var errIrohNotConfigured = errors.New("iroh endpoint used before ConfigureIroh")

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
//
// Binding is local work — sockets and keys, no peer — so it takes no context
// from its callers, none of which have one to give.
func (e *IrohEndpoint) bind() (*iroh.Endpoint, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.endpoint != nil {
		return e.endpoint, nil
	}
	preset := iroh.PresetN0
	if e.cfg.DisableRelay {
		preset = iroh.PresetN0NoRelay
	}
	// iroh takes the 32-byte seed, which is the first half of a Go ed25519
	// private key; the second half is the public key it derives anyway.
	var secret iroh.SecretKey
	copy(secret[:], e.cfg.SecretKey.Seed())

	bound, err := iroh.Bind(context.Background(), iroh.Options{
		Preset:    preset,
		SecretKey: &secret,
		ALPNs:     [][]byte{[]byte(irohALPN)},
	})
	if err != nil {
		return nil, fmt.Errorf("bind iroh endpoint: %w", err)
	}
	e.endpoint = bound
	return e.endpoint, nil
}

// close releases the bound socket. Only [IrohEndpoint.Listen]'s cleanup calls
// it: a process binds one endpoint, and a server that has stopped listening
// has no use for the socket it was listening on.
func (e *IrohEndpoint) close() {
	e.mu.Lock()
	bound := e.endpoint
	e.endpoint = nil
	e.mu.Unlock()
	if bound == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), irohTeardownTimeout)
	defer cancel()
	_ = bound.Close(ctx)
}

// DirectAddrs are the socket addresses this endpoint is reachable at, for a
// caller that must locate a peer without discovery.
func (e *IrohEndpoint) DirectAddrs() ([]string, error) {
	bound, err := e.bind()
	if err != nil {
		return nil, err
	}
	return dialableAddrs(bound)
}

// dialableAddrs are the endpoint's addresses with the unspecified ones
// rewritten to loopback.
//
// Two sources, because they answer different questions. Addr carries the
// concrete addresses discovery would publish, which is what reaches a peer
// elsewhere but can still be filling in. BoundSockets is what the OS gave us,
// which is available immediately — for the default bind that is 0.0.0.0 and
// [::], correct as a bind and useless as a dial target, so the wildcards are
// kept as loopback and a peer on this machine can still reach it. That is the
// `task dev` case.
func dialableAddrs(ep *iroh.Endpoint) ([]string, error) {
	addr, err := ep.Addr()
	if err != nil {
		return nil, fmt.Errorf("iroh endpoint address: %w", err)
	}
	sockets, err := ep.BoundSockets()
	if err != nil {
		return nil, fmt.Errorf("iroh bound sockets: %w", err)
	}

	seen := map[string]struct{}{}
	out := make([]string, 0, len(addr.DirectAddrs)+len(sockets))
	for _, candidate := range append(addr.DirectAddrs, sockets...) {
		if candidate.Addr().IsUnspecified() {
			loopback := netip.IPv6Loopback()
			if candidate.Addr().Is4() {
				loopback = netip.AddrFrom4([4]byte{127, 0, 0, 1})
			}
			candidate = netip.AddrPortFrom(loopback, candidate.Port())
		}
		text := candidate.String()
		if _, ok := seen[text]; ok {
			continue
		}
		seen[text] = struct{}{}
		out = append(out, text)
	}
	return out, nil
}

// parseIrohAddrs converts the direct addresses carried on an endpoint URL into
// the form iroh dials. A malformed one is an error rather than a silent skip:
// it is the only thing making the peer reachable without discovery, so
// dropping it would turn a typo into an unexplained timeout.
func parseIrohAddrs(addrs []string) ([]netip.AddrPort, error) {
	out := make([]netip.AddrPort, 0, len(addrs))
	for _, addr := range addrs {
		parsed, err := netip.ParseAddrPort(addr)
		if err != nil {
			return nil, fmt.Errorf("iroh direct address %q: %w", addr, err)
		}
		out = append(out, parsed)
	}
	return out, nil
}

// The process's default endpoint, installed by ConfigureIroh. Listen and
// HTTPClient resolve an iroh:// endpoint through it, so the schemes that have
// no identity to configure keep their existing call signatures.
var (
	defaultIrohMu sync.Mutex
	defaultIroh   *IrohEndpoint
)

func configureIroh(cfg IrohConfig) error {
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

func localIrohID() (IrohID, error) {
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

// irohRoundTripper serves HTTP over streams opened to one peer. Each request
// gets its own bidirectional QUIC stream, which is what the unix transport gets
// from a socket connection and what keeps hijack — and therefore websockets —
// working.
func irohRoundTripper(target Endpoint, base http.RoundTripper) (http.RoundTripper, error) {
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

	var (
		mu   sync.Mutex
		conn *iroh.Conn
	)
	// One QUIC connection per peer, redialed when it dies. Streams are cheap;
	// connections carry the handshake, the path discovery, and the NAT
	// traversal state, so tearing one down per request would pay all of that
	// again on every call.
	//
	// A caller reports the connection it found dead rather than asking for a
	// new one, which is what keeps a peer restarting from costing one
	// connection per request in flight: whoever gets here first replaces it,
	// and everyone else finds the replacement already waiting.
	dial := func(ctx context.Context, stale *iroh.Conn) (*iroh.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		if conn != nil && conn != stale {
			return conn, nil
		}
		addrs := append(append([]string(nil), direct...), locate(e.cfg.Locate, id)...)
		parsed, err := parseIrohAddrs(addrs)
		if err != nil {
			return nil, err
		}
		addr := iroh.AddrOf(iroh.EndpointID(id)).WithDirectAddrs(parsed...)
		opened, err := ep.Connect(ctx, addr, []byte(irohALPN))
		if err != nil {
			return nil, fmt.Errorf("dial iroh endpoint %s: %w", id.Short(), err)
		}
		conn = opened
		return conn, nil
	}

	transport := cloneTransport(base)
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		// A pooled connection can be gone without this side knowing yet — the
		// peer restarted, the network moved — and that is reported as a
		// connection-level error rather than a stream one. It earns exactly
		// one redial: more would retry a peer that is simply unreachable on
		// every request, and the error already says why it failed.
		var stale *iroh.Conn
		for attempt := 0; ; attempt++ {
			active, err := dial(ctx, stale)
			if err != nil {
				return nil, err
			}
			stream, err := active.OpenConn(ctx)
			if err == nil {
				return stream, nil
			}
			if attempt == 0 && errors.Is(err, iroh.ErrConnection) {
				stale = active
				continue
			}
			return nil, fmt.Errorf("open iroh stream to %s: %w", id.Short(), err)
		}
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

func locate(locator func(IrohID) []string, id IrohID) []string {
	if locator == nil {
		return nil
	}
	return locator(id)
}

// irohListen binds this machine's endpoint and presents accepted streams as a
// net.Listener, so the ordinary control-plane handler serves them unchanged.
func irohListen(Endpoint) (net.Listener, string, func(), error) {
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
	addrs, err := dialableAddrs(ep)
	if err != nil {
		return nil, "", nil, err
	}
	listener := ep.Listener(iroh.ListenOptions{Authorize: e.authorize})
	// The dialable URL is only known once the endpoint is bound, which is why
	// Listen reports the address it ended up with rather than echoing the one
	// it was given. The direct addresses ride along so the logged URL works
	// before any discovery service does.
	cleanup := func() {
		_ = listener.Close()
		e.close()
	}
	return listener, IrohURLWithAddrs(IrohID(ep.ID()), addrs), cleanup, nil
}

// authorize decides whether a peer may speak to the control plane at all. It
// runs once the handshake has proven who the peer is and before any stream of
// theirs is accepted, so an unenrolled peer never reaches the handler surface,
// and the returned error is the close reason it reads.
//
// A nil Authorize refuses everyone: a listener that admits anyone holding the
// address is an unauthenticated control plane (ADR 0052 §5).
func (e *IrohEndpoint) authorize(conn *iroh.Conn) error {
	id, err := conn.RemoteID()
	if err != nil {
		return fmt.Errorf("endpoint identity is unreadable: %w", err)
	}
	peer := IrohID(id)
	if e.cfg.Authorize == nil || !e.cfg.Authorize(peer) {
		return fmt.Errorf("endpoint %s is not authorized on this server", peer)
	}
	return nil
}
