package endpoint

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	iroh "github.com/discobox-ai/iroh-go"
)

// irohALPN is the application protocol this transport negotiates. It names the
// protocol carried on the stream, which is ordinary HTTP/1.1, so a future
// protocol on the same endpoint gets its own ALPN rather than having to be
// distinguished inside the byte stream.
const irohALPN = "discobox/http/1"

// irohCertALPN is the same control plane, reached by a client that dials with
// an ephemeral key and presents a certificate for the identity it enrolled
// (ADR 0104 §4).
//
// The ALPN is what says a certificate is coming. Nothing else in a connection
// does, and finding out by reading would consume the first stream of a client
// that was never going to send one. It also makes the negotiation explicit in
// both directions: a client meeting a server that does not serve this ALPN is
// refused at the TLS layer, which says "this server does not do certificates"
// rather than the misleading "not enrolled" it would otherwise be told.
const irohCertALPN = "discobox/http/1+cert"

// irohCertWait bounds reading a certificate from a peer that has just
// connected. Short, because the peer sends it unprompted the moment the
// handshake completes, so this waits on bytes already in flight rather than on
// a round trip — and because unlike the admission store's wait, what it is
// waiting for is a remote party.
const irohCertWait = 3 * time.Second

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

	started := time.Now()
	bound, err := iroh.Bind(context.Background(), iroh.Options{
		Preset:    preset,
		RelayMode: relay,
		RelayURLs: e.cfg.RelayURLs,
		SecretKey: &secret,
		ALPNs:     irohALPNs(e.cfg),
		BindAddrs: e.cfg.BindAddrs,
	})
	if err != nil {
		irohLogf(IrohLogError, "bind: %v", err)
		return nil, fmt.Errorf("bind iroh endpoint: %w", err)
	}
	irohLogf(IrohLogInfo, "bind: endpoint %s bound in %s (%s)",
		IrohID(bound.ID()).Short(), time.Since(started).Round(time.Millisecond), irohReachDescription(e.cfg))
	if irohLogEnabled(IrohLogDebug) {
		if sockets, socketsErr := bound.BoundSockets(); socketsErr != nil {
			irohLogf(IrohLogDebug, "bind: bound sockets are unreadable: %v", socketsErr)
		} else {
			irohLogf(IrohLogDebug, "bind: sockets %s", joinAddrPorts(sockets))
		}
	}
	e.endpoint = bound
	return e.endpoint, nil
}

// irohALPNs is what this endpoint speaks.
//
// A server serves both, so a client of either kind reaches it. A client dials
// exactly one — the certificate ALPN when it has a certificate to present, the
// plain one otherwise — because dialing is choosing, and offering both would
// leave the server to guess which kind of client this is.
// dialALPN is the protocol this endpoint asks for when it dials: the
// certificate ALPN when it has one to present, the plain one otherwise.
// Dialing is choosing, and the choice is what tells the server which kind of
// client has arrived.
func (e *IrohEndpoint) dialALPN() string {
	if e.cfg.Certificate != nil {
		return irohCertALPN
	}
	return irohALPN
}

func irohALPNs(cfg IrohConfig) [][]byte {
	if cfg.Certificate != nil {
		return [][]byte{[]byte(irohCertALPN)}
	}
	if cfg.Authorize != nil {
		return [][]byte{[]byte(irohCertALPN), []byte(irohALPN)}
	}
	return [][]byte{[]byte(irohALPN)}
}

// irohReachDescription says how this endpoint expects to find peers, in the
// two terms that decide it: whether it can look an ID up, and which relays it
// falls back to. Both are configuration a caller chose, and both are invisible
// in the error a failed dial produces.
func irohReachDescription(cfg IrohConfig) string {
	discovery := "discovery on"
	if cfg.DisableDiscovery {
		discovery = "discovery off"
	}
	switch {
	case cfg.DisableRelay:
		return discovery + ", relays off"
	case len(cfg.RelayURLs) > 0:
		return discovery + ", relays " + strings.Join(cfg.RelayURLs, " ")
	default:
		return discovery + ", default relays"
	}
}

func joinAddrPorts(addrs []netip.AddrPort) string {
	if len(addrs) == 0 {
		return "none"
	}
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, addr.String())
	}
	return strings.Join(out, " ")
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

// Relay is the relay this endpoint is reachable through, waiting until ctx
// expires for it to come online. See [LocalIrohRelay], which is this for the
// process default.
func (e *IrohEndpoint) Relay(ctx context.Context) (string, error) {
	if e.cfg.DisableRelay {
		// Online never returns on its own with relays off, so waiting on it
		// would spend the caller's entire timeout to learn something this
		// endpoint's own configuration already says.
		return "", nil
	}
	bound, err := e.bind()
	if err != nil {
		return "", err
	}
	if err := bound.Online(ctx); err != nil {
		irohLogf(IrohLogWarn, "relay: not online: %v", err)
		return "", err
	}
	home, err := bound.HomeRelay()
	if err != nil {
		return "", err
	}
	irohLogf(IrohLogInfo, "relay: home relay is %s", cmp.Or(home, "none"))
	return home, nil
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
			irohLogf(IrohLogError, "connect: %s has an unusable direct address: %v", id.Short(), err)
			return nil, err
		}
		addr := iroh.AddrOf(iroh.EndpointID(id)).WithDirectAddrs(parsed...)
		alpn := e.dialALPN()
		irohLogf(IrohLogInfo, "connect: dialing %s (direct %s, alpn %s)", id.Short(), joinAddrPorts(parsed), alpn)
		started := time.Now()
		opened, err := ep.Connect(ctx, addr, []byte(alpn))
		if err != nil {
			irohLogf(IrohLogError, "connect: %s failed after %s: %v", id.Short(), time.Since(started).Round(time.Millisecond), err)
			return nil, fmt.Errorf("dial iroh endpoint %s: %w", id.Short(), err)
		}
		// A handshake proves the peer's identity and nothing about whether it
		// will admit us: a server refuses by closing the connection after
		// accepting it, so the refusal surfaces at the first stream rather than
		// here. That is the layer boundary this line marks.
		irohLogf(IrohLogInfo, "connect: %s connected in %s", id.Short(), time.Since(started).Round(time.Millisecond))
		// Before the connection is pooled, so no request can be the thing that
		// discovers the certificate was never sent.
		if err := e.presentCertificate(ctx, opened); err != nil {
			irohLogf(IrohLogError, "connect: %s could not be given this peer's certificate: %v", id.Short(), err)
			return nil, err
		}
		conn = opened
		if e.cfg.Reached != nil {
			go e.watchPaths(id, opened)
		}
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
				irohLogf(IrohLogDebug, "stream: opened to %s", id.Short())
				return stream, nil
			}
			if attempt == 0 && errors.Is(err, iroh.ErrConnection) {
				irohLogf(IrohLogWarn, "stream: the connection to %s is gone (%v); redialing", id.Short(), err)
				stale = active
				continue
			}
			irohLogf(IrohLogError, "stream: open to %s failed: %v", id.Short(), err)
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

// reached reports the direct addresses this connection is actually using to
// [IrohConfig.Reached].
//
// The addresses come off the live connection rather than out of discovery
// because what is worth remembering is the one that works *from here*. A
// server publishes every socket it bound — its Docker bridges, its loopback,
// its VPN address — and which of them a given client can route to is a fact
// about the client, answered only by having connected.
//
// The path carrying traffic goes first, then the other direct paths in the
// order iroh opened them. A caller that keeps only a few keeps the one that
// worked, and the same connection reports the same list every time.
//
// It reports whether there is nothing more to learn from this connection:
// either it reported, or the connection can no longer say.
func (e *IrohEndpoint) reached(id IrohID, conn *iroh.Conn) bool {
	if e.cfg.Reached == nil {
		return true
	}
	paths, err := conn.Paths()
	if err != nil {
		// Debug, and no further: this is a hint for a later dial, and the
		// caller is in the middle of a request that is working.
		irohLogf(IrohLogDebug, "reached: the paths to %s are unreadable: %v", id.Short(), err)
		return true
	}
	addrs := make([]string, 0, len(paths))
	for _, path := range paths {
		if path.Kind != iroh.PathIP {
			continue
		}
		if path.Selected {
			addrs = append([]string{path.Remote}, addrs...)
		} else {
			addrs = append(addrs, path.Remote)
		}
	}
	if len(addrs) == 0 {
		return false
	}
	e.cfg.Reached(id, addrs)
	return true
}

const (
	// irohPathPoll is how often a new connection is asked whether it has a
	// direct path yet. iroh has no event for it, and the answer usually
	// arrives within a relay round trip or two of the handshake.
	irohPathPoll = 200 * time.Millisecond
	// irohPathWatch bounds the asking. A connection that has no direct path by
	// then is most likely one that will stay on the relay, and a watch that
	// ran for the life of every connection would be a goroutine per peer
	// asking a question whose answer has stopped changing.
	irohPathWatch = 30 * time.Second
)

// watchPaths reports a new connection's direct paths to [IrohConfig.Reached]
// as soon as it has one.
//
// A connection starts on the relay and moves to a direct path once hole
// punching succeeds, which is a round trip or two over the relay after the
// handshake. Nothing a request does lines up with that moment: the first
// stream opens before it, and every request after reuses that stream, so a
// hook on the request path sees a relayed connection and never looks again.
// This watches the connection instead, for as long as the process holds it —
// a command that exits first learns nothing, which is no worse than before.
func (e *IrohEndpoint) watchPaths(id IrohID, conn *iroh.Conn) {
	deadline := time.Now().Add(irohPathWatch)
	ticker := time.NewTicker(irohPathPoll)
	defer ticker.Stop()
	for !e.reached(id, conn) && time.Now().Before(deadline) {
		select {
		case <-ticker.C:
		case <-e.ctx.Done():
			return
		}
	}
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
	irohLogf(IrohLogInfo, "listen: accepting as %s (alpn %s)", IrohID(ep.ID()).Short(), irohALPN)
	cleanup := func() {
		irohLogf(IrohLogInfo, "listen: closing the endpoint")
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
		irohLogf(IrohLogWarn, "accept: the peer's identity is unreadable: %v", err)
		return fmt.Errorf("endpoint identity is unreadable: %w", err)
	}
	peer := IrohID(id)
	// Whichever identity is actually being claimed is the one the policy sees,
	// and it sees it once. A certificate-bearing peer is never first evaluated
	// as the ephemeral endpoint it dialed from, which would refuse and log it
	// as unenrolled on the way to admitting it.
	claimed := peer
	if certified, err := e.presentedIdentity(conn, peer); err != nil {
		irohLogf(IrohLogWarn, "accept: refused %s: %v", peer.Short(), err)
		return err
	} else if certified != nil {
		claimed = *certified
		irohLogf(IrohLogInfo, "accept: %s presented a certificate for %s", peer.Short(), claimed.Short())
	}
	if e.cfg.Authorize == nil {
		irohLogf(IrohLogWarn, "accept: refusing %s: this endpoint has no admission policy", peer.Short())
		return fmt.Errorf("endpoint %s is not authorized on this server", peer)
	}
	// The policy's own error is the close reason, unwrapped: it is written for
	// the operator reading it on the other end, and wrapping it here would
	// prefix every refusal with this package's framing.
	//
	// Both outcomes are logged here rather than inside the policy, because this
	// is the one place every outcome passes through: an admitted peer is
	// otherwise invisible, and a server whose log only records refusals cannot
	// answer "did my client get in at all".
	if err := e.cfg.Authorize(e.ctx, claimed); err != nil {
		irohLogf(IrohLogWarn, "accept: refused %s: %v", claimed.Short(), err)
		return err
	}
	irohLogf(IrohLogInfo, "accept: admitted %s", claimed.Short())
	return nil
}

// presentedIdentity reads the certificate a peer on the certificate ALPN owes
// us, and reports the enrolled identity it proves. A peer on the plain ALPN
// presents nothing and is its own identity, so this returns nil for it without
// touching a stream — which is what leaves an existing client's first stream
// for the HTTP listener, exactly as before (ADR 0104 §4).
func (e *IrohEndpoint) presentedIdentity(conn *iroh.Conn, peer IrohID) (*IrohID, error) {
	alpn, err := conn.ALPN()
	if err != nil {
		return nil, fmt.Errorf("this connection's protocol is unreadable: %w", err)
	}
	if string(alpn) != irohCertALPN {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(e.ctx, irohCertWait)
	defer cancel()
	stream, err := conn.AcceptConn(ctx)
	if err != nil {
		return nil, fmt.Errorf("this connection promised a peer certificate and opened no stream to carry it: %w", err)
	}
	defer func() { _ = stream.Close() }()

	raw := make([]byte, PeerCertSize)
	// The header first, so a stream carrying something else is refused on its
	// first bytes rather than parsed as a certificate.
	if _, err := io.ReadFull(stream, raw[:PeerCertHeaderSize]); err != nil {
		return nil, fmt.Errorf("reading the peer certificate: %w", err)
	}
	if err := CheckPeerCertHeader(raw[:PeerCertHeaderSize]); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(stream, raw[PeerCertHeaderSize:]); err != nil {
		return nil, fmt.Errorf("reading the peer certificate: %w", err)
	}
	cert, err := ParsePeerCert(raw)
	if err != nil {
		return nil, err
	}
	// The comparison the design rests on: a certificate is only bytes, and it
	// means something only because the handshake proved this peer holds the key
	// it names.
	if err := cert.Check(peer, time.Now()); err != nil {
		return nil, err
	}
	return &cert.Issuer, nil
}

// presentCertificate is the client half: the certificate goes out unprompted
// the moment the connection is up, on a stream of its own.
//
// Nothing is waited for. The server either admits the connection or closes it
// with a reason, and a client that waited for an acknowledgement would pay a
// round trip to learn what its next request tells it anyway.
func (e *IrohEndpoint) presentCertificate(ctx context.Context, conn *iroh.Conn) error {
	if e.cfg.Certificate == nil {
		return nil
	}
	raw, err := e.cfg.Certificate.MarshalBinary()
	if err != nil {
		return err
	}
	certCtx, cancel := context.WithTimeout(ctx, irohCertWait)
	defer cancel()
	stream, err := conn.OpenConn(certCtx)
	if err != nil {
		return fmt.Errorf("open the stream for this peer's certificate: %w", err)
	}
	defer func() { _ = stream.Close() }()
	if _, err := stream.Write(raw); err != nil {
		return fmt.Errorf("send this peer's certificate: %w", err)
	}
	return nil
}

// IrohListenerState is what this process's iroh endpoint can say about itself:
// the identity peers dial, whether it is currently reachable through a relay,
// and the sockets and addresses it answers on.
//
// It exists because a server whose iroh listener has quietly stopped working
// looks exactly like one whose listener is fine. The process is up, its unix
// socket answers, /healthz says ready — and every client dialing its peer ID
// times out with nothing anywhere to say why. This is the answer to that,
// asked of the transport by the process that owns it.
type IrohListenerState struct {
	// ID is the identity peers dial. It cannot change while the process runs.
	ID IrohID
	// HomeRelay is the relay this endpoint is reachable through, empty when it
	// is not reachable through one.
	HomeRelay string
	// Online reports whether the endpoint has a relay right now. A listener
	// that is not online is reachable only from networks that can route to its
	// sockets directly, which is a working deployment and a completely
	// different one from what its address implies.
	Online bool
	// Sockets are the local UDP addresses the endpoint is bound to.
	Sockets []netip.AddrPort
	// DirectAddrs are the addresses it believes peers can reach it at.
	DirectAddrs []string
}

// LocalIrohListenerState reports what this process's iroh endpoint can say
// about itself. See [IrohListenerState].
//
// ctx bounds the one part of this that waits: whether a relay is reached. Pass
// a short deadline — this is a status probe, and "not online within a moment"
// is the answer a caller wants rather than something to block on.
func LocalIrohListenerState(ctx context.Context) (IrohListenerState, error) {
	configured, err := defaultIrohEndpoint()
	if err != nil {
		return IrohListenerState{}, err
	}
	return configured.listenerState(ctx)
}

func (e *IrohEndpoint) listenerState(ctx context.Context) (IrohListenerState, error) {
	id, err := e.ID()
	if err != nil {
		return IrohListenerState{}, err
	}
	state := IrohListenerState{ID: id}
	bound, err := e.bind()
	if err != nil {
		return state, err
	}
	if sockets, socketsErr := bound.BoundSockets(); socketsErr == nil {
		state.Sockets = sockets
	}
	if addrs, addrsErr := e.DirectAddrs(); addrsErr == nil {
		state.DirectAddrs = addrs
	}
	// Last, and allowed to fail: a relay that is not reached is the finding,
	// not an error. Everything above it is still worth reporting.
	if relay, relayErr := e.Relay(ctx); relayErr == nil {
		state.HomeRelay = relay
		state.Online = true
	}
	return state, nil
}
