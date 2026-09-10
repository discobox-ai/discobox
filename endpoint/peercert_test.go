package endpoint

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

func testCert(t *testing.T) (ed25519.PrivateKey, IrohID, PeerCert) {
	t.Helper()
	_, issuer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate issuer: %v", err)
	}
	subjectPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate subject: %v", err)
	}
	subject, err := IrohIDFromPublicKey(subjectPub)
	if err != nil {
		t.Fatalf("IrohIDFromPublicKey() error = %v", err)
	}
	cert, err := SignPeerCert(issuer, subject, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SignPeerCert() error = %v", err)
	}
	return issuer, subject, cert
}

func TestPeerCertRoundTrips(t *testing.T) {
	_, subject, cert := testCert(t)
	raw, err := cert.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary() error = %v", err)
	}
	if len(raw) != PeerCertSize {
		t.Fatalf("certificate is %d bytes, want the %d this format fixes", len(raw), PeerCertSize)
	}
	parsed, err := ParsePeerCert(raw)
	if err != nil {
		t.Fatalf("ParsePeerCert() error = %v", err)
	}
	if parsed.Issuer != cert.Issuer || parsed.Subject != subject {
		t.Fatalf("parsed %+v, want issuer %s subject %s", parsed, cert.Issuer, subject)
	}
	if err := parsed.Check(subject, time.Now()); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
}

// The line the whole design rests on: a certificate is only bytes, so it has to
// be worthless to anyone but the endpoint it names. Presenting a valid,
// correctly signed certificate from a different endpoint must be refused.
func TestPeerCertIsRefusedFromAnotherEndpoint(t *testing.T) {
	_, _, cert := testCert(t)
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	other, err := IrohIDFromPublicKey(otherPub)
	if err != nil {
		t.Fatalf("IrohIDFromPublicKey() error = %v", err)
	}
	err = cert.Check(other, time.Now())
	if err == nil {
		t.Fatal("a certificate issued for one endpoint was accepted from another")
	}
	if !strings.Contains(err.Error(), "arrived from") {
		t.Fatalf("error = %v, want it to name both endpoints", err)
	}
}

// Every byte the signature covers must be covered. A tampered field has to fail
// verification rather than parse into a certificate that says something else.
func TestPeerCertRejectsTampering(t *testing.T) {
	_, _, cert := testCert(t)
	raw, err := cert.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary() error = %v", err)
	}
	for _, tc := range []struct {
		name string
		at   int
	}{
		{"version", len("DBXPEER")},
		{"issuer", PeerCertHeaderSize},
		{"subject", PeerCertHeaderSize + IrohIDSize},
		{"expiry", PeerCertHeaderSize + 2*IrohIDSize},
		{"signature", PeerCertSize - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tampered := append([]byte(nil), raw...)
			tampered[tc.at] ^= 0xff
			if _, err := ParsePeerCert(tampered); err == nil {
				t.Fatalf("a certificate with a tampered %s parsed cleanly", tc.name)
			}
		})
	}
}

func TestPeerCertExpires(t *testing.T) {
	_, subject, _ := testCert(t)
	_, issuer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	expired, err := SignPeerCert(issuer, subject, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("SignPeerCert() error = %v", err)
	}
	if err := expired.Check(subject, time.Now()); err == nil {
		t.Fatal("an expired certificate was accepted")
	}
}

// A stream carrying something else — an HTTP request from a client that should
// not be on this ALPN — is refused on its first bytes rather than parsed.
func TestPeerCertHeaderRefusesOtherTraffic(t *testing.T) {
	if err := CheckPeerCertHeader([]byte("GET /api/")); err == nil {
		t.Fatal("an HTTP request line was accepted as a certificate header")
	}
	if err := CheckPeerCertHeader([]byte("DBX")); err == nil {
		t.Fatal("a short read was accepted as a certificate header")
	}
	_, _, cert := testCert(t)
	raw, _ := cert.MarshalBinary()
	if err := CheckPeerCertHeader(raw[:PeerCertHeaderSize]); err != nil {
		t.Fatalf("CheckPeerCertHeader() on a real certificate = %v", err)
	}
}
