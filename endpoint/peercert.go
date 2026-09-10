package endpoint

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// A client dials with a key it generated at startup and throws away, and
// proves the identity an operator enrolled by presenting a certificate for it
// (ADR 0104).
//
// The two jobs one key used to do pull in opposite directions: the enrolled
// credential wants to be stable and machine-wide, while the address a live
// endpoint dials from must be unique per endpoint — a relay keeps one active
// connection per endpoint ID, so two processes sharing one identity take it
// from each other and neither completes a handshake.
//
// The certificate is not transferable, and that is the whole of its security:
// it names one ephemeral public key, and the QUIC handshake independently
// proves the peer holds that key's private half. A stolen certificate is worth
// nothing without a private key that never leaves the process that made it.

const (
	// peerCertMagic is the first field, so a stream carrying something else —
	// an HTTP request from a client that should not have been on this ALPN —
	// is refused on its first bytes rather than parsed as a certificate.
	peerCertMagic = "DBXPEER"
	// PeerCertVersion is the format below. It is inside the signed input, so a
	// signature made for one version cannot be replayed as another.
	PeerCertVersion = 1

	// PeerCertSize is the whole record: magic, version, issuer, subject,
	// expiry, signature. Fixed width rather than length-prefixed, so reading
	// it is two reads and there is no length for the two ends to disagree
	// about.
	PeerCertSize = 144
	// peerCertSignedSize is the prefix the signature covers: everything but
	// the signature itself.
	peerCertSignedSize = PeerCertSize - ed25519.SignatureSize
)

// PeerCert binds an ephemeral transport key to an enrolled identity.
type PeerCert struct {
	// Issuer is the enrolled identity: the key an operator added with
	// `discobox admin peer add`, and the one the allowlist is checked against.
	Issuer IrohID
	// Subject is the ephemeral endpoint ID this certificate is for. A verifier
	// must compare it against the identity the handshake proved, or the
	// certificate becomes transferable and the scheme fails open.
	Subject IrohID
	// Expiry bounds the damage from a process compromised while running. A
	// certificate is already useless without the ephemeral private key.
	Expiry time.Time

	signature [ed25519.SignatureSize]byte
}

// SignPeerCert issues a certificate for subject, signed by the enrolled key.
func SignPeerCert(issuer ed25519.PrivateKey, subject IrohID, expiry time.Time) (PeerCert, error) {
	public, ok := issuer.Public().(ed25519.PublicKey)
	if !ok {
		return PeerCert{}, errors.New("issuer key is not ed25519")
	}
	id, err := IrohIDFromPublicKey(public)
	if err != nil {
		return PeerCert{}, err
	}
	cert := PeerCert{Issuer: id, Subject: subject, Expiry: expiry.UTC().Truncate(time.Second)}
	copy(cert.signature[:], ed25519.Sign(issuer, cert.signed()))
	return cert, nil
}

// signed is the input the signature covers: the whole record but the signature.
// The magic and version are inside it, which is what keeps a signature made for
// this purpose from being lifted into another one.
func (c PeerCert) signed() []byte {
	out := make([]byte, peerCertSignedSize)
	copy(out, peerCertMagic)
	out[len(peerCertMagic)] = PeerCertVersion
	at := len(peerCertMagic) + 1
	at += copy(out[at:], c.Issuer[:])
	at += copy(out[at:], c.Subject[:])
	binary.BigEndian.PutUint64(out[at:], uint64(c.Expiry.Unix()))
	return out
}

// MarshalBinary renders the wire form: exactly [PeerCertSize] bytes.
func (c PeerCert) MarshalBinary() ([]byte, error) {
	out := make([]byte, 0, PeerCertSize)
	out = append(out, c.signed()...)
	return append(out, c.signature[:]...), nil
}

// PeerCertHeaderSize is the prefix a reader can check before committing to the
// rest: the magic and the version.
const PeerCertHeaderSize = len(peerCertMagic) + 1

// CheckPeerCertHeader reports whether these bytes begin a certificate this
// build can read, so a reader can refuse a stream carrying something else
// before reading any further.
func CheckPeerCertHeader(header []byte) error {
	if len(header) < PeerCertHeaderSize {
		return fmt.Errorf("a peer certificate starts with %d bytes, got %d", PeerCertHeaderSize, len(header))
	}
	if string(header[:len(peerCertMagic)]) != peerCertMagic {
		return errors.New("this is not a peer certificate")
	}
	if version := header[len(peerCertMagic)]; version != PeerCertVersion {
		return fmt.Errorf("peer certificate version %d, this server reads version %d", version, PeerCertVersion)
	}
	return nil
}

// ParsePeerCert reads the wire form. It checks the shape and the signature, and
// deliberately not the expiry or the subject: those are the verifier's
// decisions, and they need the connection and the clock this parser does not
// have. See [PeerCert.Check].
func ParsePeerCert(raw []byte) (PeerCert, error) {
	if len(raw) != PeerCertSize {
		return PeerCert{}, fmt.Errorf("a peer certificate is %d bytes, got %d", PeerCertSize, len(raw))
	}
	if err := CheckPeerCertHeader(raw); err != nil {
		return PeerCert{}, err
	}
	var cert PeerCert
	at := PeerCertHeaderSize
	at += copy(cert.Issuer[:], raw[at:at+IrohIDSize])
	at += copy(cert.Subject[:], raw[at:at+IrohIDSize])
	cert.Expiry = time.Unix(int64(binary.BigEndian.Uint64(raw[at:at+8])), 0).UTC()
	copy(cert.signature[:], raw[peerCertSignedSize:])
	if !ed25519.Verify(cert.Issuer.PublicKey(), cert.signed(), cert.signature[:]) {
		return PeerCert{}, errors.New("this peer certificate is not signed by the identity it names")
	}
	return cert, nil
}

// Check is what a verifier asks once it has the connection: that this
// certificate is for the peer that presented it, and that it has not expired.
//
// The subject comparison is the line the whole design rests on. Without it a
// certificate could be replayed by anyone who captured one, because a
// certificate is only bytes; with it, presenting one means nothing unless the
// handshake also proved possession of the key it names.
func (c PeerCert) Check(subject IrohID, now time.Time) error {
	if c.Subject != subject {
		return fmt.Errorf("this peer certificate is for %s, and it arrived from %s", c.Subject.Short(), subject.Short())
	}
	if !c.Expiry.IsZero() && now.After(c.Expiry) {
		return fmt.Errorf("this peer certificate expired at %s", c.Expiry.Format(time.RFC3339))
	}
	return nil
}
