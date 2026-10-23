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
	// ever produce the first (ADR 0095 enrolled iroh IDs §4).
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

	// Reached is told the socket addresses a peer turned out to answer on, so
	// a caller that keeps them can hand them back through Locate next time and
	// dial the peer directly instead of waiting on discovery and a relay.
	//
	// It is the write half of Locate and exists because discovery publishes a
	// peer's relay and nothing else: the only way to learn an address that
	// works from this machine is to have used one. Nil keeps nothing, which is
	// what a peer dialed once wants.
	Reached func(IrohID, []string)

	// RelayURLs are the relay servers to use instead of the defaults, for a
	// deployment running its own (ADR 0096 server config §6). Empty keeps n0's public
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

	// Issuer is the enrolled key this ephemeral endpoint speaks for (ADR 0104).
	// Set, the endpoint signs a fresh certificate for its own SecretKey on every
	// dial and presents it, so the identity a server checks is the issuer's.
	//
	// The key rather than a certificate, because a certificate expires
	// (PeerCertLifetime) and a process does not: one signed at startup is
	// refused on the first redial after it lapses, which a console left open
	// overnight reaches. Signing per dial keeps the process working for as long
	// as it runs.
	//
	// Set it and SecretKey becomes a key generated for this process alone. Nil
	// keeps the old arrangement, where the key that dials is the key that was
	// enrolled — which is what every client did before certificates existed and
	// what one still does against a server that does not offer them.
	//
	// A client sets this. A server does not: its own identity is its address.
	Issuer ed25519.PrivateKey

	// BindAddrs are the local UDP addresses to bind, replacing the default
	// wildcard sockets. Empty binds the default, which is what a process
	// reachable from another machine wants.
	//
	// It exists for the endpoint that is only ever reached from this machine.
	// A wildcard bind is what makes Windows Firewall raise its prompt, and a
	// test binary is a fresh path under a temporary directory on every run, so
	// no allowance a developer grants is ever asked about again.
	BindAddrs []netip.AddrPort

	// PreferredBindAddrs are sockets to try before the ones BindAddrs, or the
	// default, would give. A family those would bind and these leave out is
	// bound anyway, on a fresh port, so the preference moves ports and never
	// drops a family. If they cannot be bound — a port was taken while this
	// process was down, an interface has gone — the endpoint binds as though
	// they had never been offered. Empty binds as before.
	//
	// It exists for a server coming back on the port it had. The port is the
	// one part of a server's address a client learns only by connecting (see
	// Reached), and a fresh one on every start throws away what every client
	// learned at the moment all of them are reconnecting at once.
	PreferredBindAddrs []netip.AddrPort

	// Bound is told the sockets the endpoint bound, once, when it binds, so a
	// caller that keeps them can offer them back through PreferredBindAddrs on
	// the next start. It runs inside the bind and must not use the endpoint.
	// Nil keeps nothing, which is what anything that does not listen wants.
	Bound func([]netip.AddrPort)
}

// ConfigureIroh installs this process's iroh identity and admission policy. It
// must be called before dialing or listening on an iroh endpoint, and only
// once: the endpoint's identity is its address, so replacing it mid-run would
// move the server while clients are talking to it.
func ConfigureIroh(cfg IrohConfig) error {
	return configureIroh(cfg)
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
