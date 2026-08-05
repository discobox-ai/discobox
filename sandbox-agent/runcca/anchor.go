package runcca

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
)

// anchorPrefix names a CA dropped into a distro's "source anchor" directory.
const anchorPrefix = "discobox-mitm-"

// AnchorFileName returns the filename a CA takes in a trust-anchor directory.
//
// The name is derived from the certificate itself rather than fixed, because a
// filesystem can legitimately hold more than one Discobox MITM CA: there is one
// per pool, and a sandbox running inside another sandbox sees both its own
// pool's CA and the CA its host injects. A single fixed name makes those two
// collide -- and because discobox-trust-ca.service installs its copy with
// `install`, which unlinks before writing, a collision against a bind mount
// fails with EBUSY and leaves the inner sandbox trusting the wrong CA.
//
// Deriving the name from the content also makes writing it idempotent: the same
// CA always lands on the same filename, and two different CAs never contend.
//
// The .crt suffix is required by update-ca-certificates, which ignores
// anything else in these directories.
func AnchorFileName(ca []byte) (string, error) {
	der, err := firstCertificateDER(ca)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return anchorPrefix + hex.EncodeToString(sum[:4]) + ".crt", nil
}

// firstCertificateDER returns the DER bytes of the first CERTIFICATE block,
// so the digest matches `openssl x509 -noout -fingerprint -sha256` over the
// same file.
func firstCertificateDER(pemBytes []byte) ([]byte, error) {
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("no PEM CERTIFICATE block found")
		}
		if block.Type == "CERTIFICATE" {
			return block.Bytes, nil
		}
	}
}

// AnchorFileNameFor is AnchorFileName with the read folded in, for callers
// that have a path rather than bytes.
func AnchorFileNameFor(ca []byte) string {
	name, err := AnchorFileName(ca)
	if err != nil {
		// A CA we cannot parse still needs a stable, unique-enough name; fall
		// back to a digest of the raw bytes rather than failing a container.
		sum := sha256.Sum256(ca)
		return fmt.Sprintf("%s%s.crt", anchorPrefix, hex.EncodeToString(sum[:4]))
	}
	return name
}
