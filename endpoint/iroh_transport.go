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

	// ctx bounds every admission check this endpoint runs, and cancel fires
	// when it closes. IrohConfig.Authorize may wait for something the process
	// has not built yet — the control plane's admission gate waits for its
	// database (ADR 0095 §4) — and a waiter with no cancellation outlives a
	// failed startup instead of being refused by it.
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	endpoint *iroh.Endpoint
	closed   bool
}

// NewIrohEndpoint prepares an endpoint. Binding is deferred to first use, so
// constructing one opens no socket.
func NewIrohEndpoint(cfg IrohConfig) (*IrohEndpoint, error) {
	if len(cfg.SecretKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("iroh secret key is %d bytes, want %d", len(cfg.SecretKey), ed25519.PrivateKeySize)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &IrohEndpoint{cfg: cfg, ctx: ctx, cancel: cancel}, nil
}

// ID is the endpoint ID peers dial to reach this endpoint.
func (e *IrohEndpoint) ID() (IrohID, error) {
	pub, ok := e.cfg.SecretKey.Public().(ed25519.PublicKey)
	if !ok {
		return IrohID{}, errors.New("iroh secret key is not ed25519")
	}
	return IrohIDFromPublicKey(pub)
}

// irohPreset turns the two switches on IrohConfig into the preset that carries
// them, plus the relay override the preset alone cannot express.
//
// iroh bundles relays and address lookup into presets rather than offering a
// flag for each: N0 has both, N0DisableRelay drops the relays, and Minimal has
// neither. Three presets do not cover four combinations, so the one that asks
// for relays without discovery starts from Minimal and puts the relays back —
// rather than quietly ignoring one of the two switches it was handed.
func irohPreset(cfg IrohConfig) (iroh.Preset, iroh.RelayMode) {
	switch {
	case cfg.DisableDiscovery && cfg.DisableRelay:
		return iroh.PresetMinimal, iroh.RelayFromPreset
	case cfg.DisableDiscovery && len(cfg.RelayURLs) > 0:
		// Minimal brings no relays, so the custom list is what supplies them.
		return iroh.PresetMinimal, iroh.RelayCustom
	case cfg.DisableDiscovery:
		return iroh.PresetMinimal, iroh.RelayDefault
	case cfg.DisableRelay:
		return iroh.PresetN0NoRelay, iroh.RelayFromPreset
	case len(cfg.RelayURLs) > 0:
		return iroh.PresetN0, iroh.RelayCustom
	default:
		return iroh.PresetN0, iroh.RelayFromPreset
	}
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
	if e.closed {
		return nil, errIrohEndpointClosed
	}
	preset, relay := irohPreset(e.cfg)
	// iroh takes the 32-byte seed, which is the first half of a Go ed25519
	// private key; the second half is the public key it derives anyway.
	var secret iroh.SecretKey
	copy(secret[:], e.cfg.SecretKey.Seed())

	bound, err := iroh.Bind(context.Background(), iroh.Options{
		Preset:    preset,
		RelayMode: relay,
		RelayURLs: e.cfg.RelayURLs,
		SecretKey: &secret,
		ALPNs:     [][]byte{[]byte(irohALPN)},
		BindAddrs: e.cfg.BindAddrs,
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
	// Before the socket, so an admission check still waiting is refused rather
	// than left parked on a listener that is going away.
	e.cancel()
	e.mu.Lock()
	bound := e.endpoint
	e.endpoint = nil
	// Closing is final. bind is lazy, so without this a second Listen would
	// re-bind a socket and hand every admission check the context close just
	// canceled — a listener that comes up and refuses every peer with "this
	// server is shutting down", on a server that is running.
	e.closed = true
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

// irohPeerFromAddr reads the endpoint ID out of an accepted connection's
// address.
//
// It takes the typed value rather than parsing Addr.String(), which renders
// iroh's own hex form. That string is a transport detail and not a peer ID
// (ADR 0097 §6): reading it with ParseIrohID would couple this seam to an
// encoding neither side owns, and would have silently reported every peer as
// the zero ID the moment the peer ID format changed.
func irohPeerFromAddr(addr net.Addr) (IrohID, bool) {
	typed, ok := addr.(iroh.Addr)
	if !ok || typed.ID.IsZero() {
		return IrohID{}, false
	}
	return IrohID(typed.ID), true
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

// errIrohEndpointClosed is returned by an endpoint that has been closed. An
// identity and its socket do not come back: build another endpoint.
var errIrohEndpointClosed = errors.New("this iroh endpoint is closed")

func localIrohFallbackURL() (string, error) {
	configured, err := defaultIrohEndpoint()
	if err != nil {
		return "", err
	}
	return configured.fallbackURL()
}

// fallbackURL is this endpoint's address with its direct socket addresses
// attached. See [LocalIrohFallbackURL], which is this for the process default.
func (e *IrohEndpoint) fallbackURL() (string, error) {
	configured := e
	id, err := configured.ID()
	if err != nil {
		return "", err
	}
	addrs, err := configured.DirectAddrs()
	if err != nil {
		return "", err
	}
	if len(addrs) == 0 {
		return "", errors.New("this endpoint has no direct addresses yet")
	}
	return IrohURLWithAddrs(id, addrs), nil
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
	listener := ep.Listener(iroh.ListenOptions{Authorize: e.authorize})
	// The address is only known once the endpoint is bound, which is why
	// Listen reports what it ended up with rather than echoing what it was
	// given.
	//
	// Direct addresses are deliberately not advertised (ADR 0097 §3). They are
	// this host's socket addresses, which in practice means its Docker bridges
	// and loopback — useless to the peer being handed the address, and a
	// description of the host's internal network to anyone the log reaches.
	// Discovery resolves a peer ID on its own; a deployment without discovery
	// writes ?addr= into the endpoint it dials, which Parse still accepts.
	cleanup := func() {
		_ = listener.Close()
		e.close()
	}
	return listener, IrohURL(IrohID(ep.ID())), cleanup, nil
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
	if e.cfg.Authorize == nil {
		return fmt.Errorf("endpoint %s is not authorized on this server", peer)
	}
	// The policy's own error is the close reason, unwrapped: it is written for
	// the operator reading it on the other end, and wrapping it here would
	// prefix every refusal with this package's framing.
	return e.cfg.Authorize(e.ctx, peer)
}
