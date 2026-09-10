package endpoint

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net"
	"net/netip"
	"net/url"
)

// IrohIDSize is the length in bytes of an iroh endpoint ID, which is an ed25519
// public key.
const IrohIDSize = ed25519.PublicKeySize

// IrohID is the identity an iroh peer is addressed by: its ed25519 public key.
//
// The address and the identity are the same value, which is what makes dialing
// one equivalent to pinning it — the TLS handshake fails unless the peer holds
// the matching secret. There is no certificate authority and no
// trust-on-first-use step to get wrong.
//
// Its text form is the peer ID (ADR 0097 §1): `d1-` and Crockford base32 with
// check symbols, which is what an operator reads, types and enrolls. See
// peerid.go.
type IrohID [IrohIDSize]byte

// IrohIDFromPublicKey converts an ed25519 public key into the endpoint ID that
// addresses its holder.
func IrohIDFromPublicKey(pub ed25519.PublicKey) (IrohID, error) {
	var id IrohID
	if len(pub) != IrohIDSize {
		return IrohID{}, fmt.Errorf("ed25519 public key is %d bytes, want %d", len(pub), IrohIDSize)
	}
	copy(id[:], pub)
	return id, nil
}

// IsZero reports whether the ID is unset.
func (id IrohID) IsZero() bool {
	return id == IrohID{}
}

// PublicKey returns the ed25519 public key the ID is made of.
func (id IrohID) PublicKey() ed25519.PublicKey {
	out := make(ed25519.PublicKey, IrohIDSize)
	copy(out, id[:])
	return out
}

// irohNetwork is the [net.Addr] network name an iroh connection reports. It
// is the one thing that identifies such a connection without naming the
// transport's types, which only a build carrying iroh has.
const irohNetwork = "iroh"

// IrohPeer reports the endpoint ID at the far end of an accepted connection,
// and whether there is one.
//
// This is the seam an authenticator reads: the peer's identity is proven by
// the QUIC handshake before the connection ever reaches a handler, so a
// principal can be built from the connection rather than from anything the
// client claims. It reads the identity off the address rather than the
// concrete type, so a caller does not have to name the transport's types to
// ask, and a connection from any other scheme simply answers false.
func IrohPeer(conn net.Conn) (IrohID, bool) {
	if conn == nil {
		return IrohID{}, false
	}
	addr := conn.RemoteAddr()
	if addr == nil || addr.Network() != irohNetwork {
		return IrohID{}, false
	}
	return irohPeerFromAddr(addr)
}

// IrohURL renders the address that dials id: the one a server prints and a
// user is handed (ADR 0097 §1).
func IrohURL(id IrohID) string {
	return SchemeDiscobox + "://" + id.String()
}

// IrohURLWithAddrs renders the address that dials id, carrying direct socket
// addresses for a peer that cannot resolve the peer ID through discovery.
//
// This is the fallback form, not the address (ADR 0097 §3): a server
// advertises the plain one and offers this beside it, because the addresses
// are its own sockets — often its Docker bridges — which are noise to a peer
// that has discovery and the only way in for a peer that does not.
func IrohURLWithAddrs(id IrohID, addrs []string) string {
	base := IrohURL(id)
	if len(addrs) == 0 {
		return base
	}
	query := url.Values{}
	for _, addr := range addrs {
		query.Add("addr", addr)
	}
	return base + "?" + query.Encode()
}

// IrohConfig is the identity and admission policy of this process's iroh
// endpoint.
type IrohConfig struct {
	// SecretKey is the ed25519 key this process answers as. Its public half is
	// the endpoint ID peers dial.
	SecretKey ed25519.PrivateKey

	// Authorize decides whether a peer may connect. It is consulted at accept,
	// before any HTTP exists, so an unenrolled peer never reaches the handler
	// surface. A nil Authorize refuses everyone: a listener that admits anyone
	// holding the address is an unauthenticated control plane (ADR 0052 §5),
	// and defaulting to open would make that the easy mistake.
	//
	// The returned error is the close reason the peer reads, so a refusal can
	// say which one it is: "not enrolled" and "this server has not finished
	// starting" send an operator to different places, and a bool could only
	// ever produce the first (ADR 0095 §4).
	//
	// The context is the listener's, not the connection's — iroh hands the
	// accept hook a connection and nothing else. It is canceled when the
	// endpoint closes, which is what releases an admission check waiting on
	// something startup has not produced yet.
	Authorize func(ctx context.Context, id IrohID) error

	// Locate returns socket addresses to try for a peer, for deployments that
	// reach peers without the default discovery service — a self-hosted setup,
	// or two peers on one host. Nil relies on discovery alone.
	Locate func(IrohID) []string

	// RelayURLs are the relay servers to use instead of the defaults, for a
	// deployment running its own (ADR 0096 §6). Empty keeps n0's public
	// relays, which are free, rate-limited, and carry no uptime guarantee.
	//
	// Both ends of a connection need this. An address carries a peer ID and
	// nothing else, so a server moved onto its own relays does not move its
	// clients with it, and a half-configured pair fails at connect time.
	//
	// It is ignored when DisableRelay is set: a caller that asked for no
	// relays has asked for something more specific than which relays.
	RelayURLs []string

	// DisableRelay binds without relay servers, for callers that must not
	// depend on anyone else's infrastructure.
	//
	// It is half the story on its own: the preset it selects drops the relays
	// and keeps n0's DNS and pkarr address lookup, which is someone else's
	// infrastructure by the same argument. Pair it with DisableDiscovery for an
	// endpoint that reaches for nothing.
	DisableRelay bool

	// DisableDiscovery binds without address lookup, for an endpoint whose
	// peers are all located some other way — Locate, or an address carried in
	// the URL. Discovery is what an endpoint consults on its first dial when it
	// has only an ID to go on, and it is a network round trip to n0.
	DisableDiscovery bool

	// Certificate proves, to a server that admits identities rather than
	// endpoints, that this ephemeral endpoint speaks for an enrolled one
	// (ADR 0100).
	//
	// Set it and SecretKey becomes a key generated for this process alone,
	// while the identity an operator enrolled is the certificate's issuer. Nil
	// keeps the old arrangement, where the key that dials is the key that was
	// enrolled — which is what every client did before certificates existed and
	// what one still does against a server that does not offer them.
	//
	// A client sets this. A server does not: its own identity is its address.
	Certificate *PeerCert

	// BindAddrs are the local UDP addresses to bind, replacing the default
	// wildcard sockets. Empty binds the default, which is what a process
	// reachable from another machine wants.
	//
	// It exists for the endpoint that is only ever reached from this machine.
	// A wildcard bind is what makes Windows Firewall raise its prompt, and a
	// test binary is a fresh path under a temporary directory on every run, so
	// no allowance a developer grants is ever asked about again.
	BindAddrs []netip.AddrPort
}

// ConfigureIroh installs this process's iroh identity and admission policy. It
// must be called before dialing or listening on an iroh endpoint, and only
// once: the endpoint's identity is its address, so replacing it mid-run would
// move the server while clients are talking to it.
func ConfigureIroh(cfg IrohConfig) error {
	return configureIroh(cfg)
}

// LocalIrohID is the endpoint ID this process answers as.
func LocalIrohID() (IrohID, error) {
	return localIrohID()
}

// LocalIrohRelay is the relay this process's endpoint is reachable through,
// waiting until ctx expires for it to come online.
//
// It answers the question a server operator cannot otherwise ask: whether this
// machine is reachable from another network at all. A peer ID resolves to a
// relay, so an endpoint that never reaches one is reachable only from networks
// that can route to its sockets directly — which is a working setup, and a
// completely different one from the one its address implies.
//
// An empty string with no error means there is no home relay to report: an
// endpoint configured without relays, which answers immediately, or one that
// came online without being given one.
func LocalIrohRelay(ctx context.Context) (string, error) {
	configured, err := defaultIrohEndpoint()
	if err != nil {
		return "", err
	}
	return configured.Relay(ctx)
}

// LocalIrohFallbackURL is this process's address with its direct socket
// addresses attached, for a peer that cannot use discovery.
//
// It is a separate call rather than something [Listen] returns because it is a
// separate thing: [Listen] reports the address, and this reports the way in
// when resolving that address does not work. A caller that has no iroh
// endpoint configured gets an error and should print nothing.
func LocalIrohFallbackURL() (string, error) {
	return localIrohFallbackURL()
}
