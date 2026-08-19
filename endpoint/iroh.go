package endpoint

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
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
// The text form is lowercase hex, the encoding upstream iroh renders and parses,
// so an ID written down here is the same string every other iroh implementation
// accepts.
type IrohID [IrohIDSize]byte

// ParseIrohID decodes an endpoint ID from its hex text form. It is deliberately
// strict about length: a truncated ID is a different identity, not a prefix of
// this one, and accepting one would let a typo silently address nobody.
func ParseIrohID(value string) (IrohID, error) {
	var id IrohID
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if trimmed == "" {
		return IrohID{}, fmt.Errorf("iroh endpoint ID is required")
	}
	decoded, err := hex.DecodeString(trimmed)
	if err != nil {
		return IrohID{}, fmt.Errorf("iroh endpoint ID %q is not hex: %w", value, err)
	}
	if len(decoded) != IrohIDSize {
		return IrohID{}, fmt.Errorf("iroh endpoint ID %q is %d bytes, want %d", value, len(decoded), IrohIDSize)
	}
	copy(id[:], decoded)
	return id, nil
}

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

// String renders the ID in the lowercase hex form iroh uses.
func (id IrohID) String() string {
	return hex.EncodeToString(id[:])
}

// Short renders the first bytes of the ID, for logs and prompts where the full
// value is noise. It is never a valid address: [ParseIrohID] rejects it, so a
// short form cannot be pasted somewhere that expects the real thing.
func (id IrohID) Short() string {
	return hex.EncodeToString(id[:5])
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

// IrohURL renders the endpoint URL that dials id.
func IrohURL(id IrohID) string {
	return "iroh://" + id.String()
}

// IrohURLWithAddrs renders the endpoint URL that dials id, carrying direct
// socket addresses for callers that cannot resolve the ID through discovery.
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
// endpoint. It is declared in every build so that callers wiring it up need no
// build tag of their own; a binary without iroh support rejects
// [ConfigureIroh] rather than failing to compile around it.
type IrohConfig struct {
	// SecretKey is the ed25519 key this process answers as. Its public half is
	// the endpoint ID peers dial.
	SecretKey ed25519.PrivateKey

	// Authorize reports whether a peer may connect. It is consulted at accept,
	// before any HTTP exists, so an unenrolled peer never reaches the handler
	// surface. A nil Authorize refuses everyone: a listener that admits anyone
	// holding the address is an unauthenticated control plane (ADR 0052 §5),
	// and defaulting to open would make that the easy mistake.
	Authorize func(IrohID) bool

	// Locate returns socket addresses to try for a peer, for deployments that
	// reach peers without the default discovery service — a self-hosted setup,
	// or two peers on one host. Nil relies on discovery alone.
	Locate func(IrohID) []string

	// DisableRelay binds without relay servers, for callers that must not
	// depend on anyone else's infrastructure.
	DisableRelay bool
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
