package proxy

import (
	"crypto/x509"
	"testing"
)

// A client that asks for a fully qualified name (CONNECT host.:443) gets a
// leaf for the name without its trailing dot: a dNSName SAN never carries one
// (RFC 5280), and a strict verifier refuses a leaf that does.
func TestMITMCertificateForTrailingDotHostOmitsTheDot(t *testing.T) {
	prepared, err := PrepareCertificates(PrepareOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("PrepareCertificates() error = %v", err)
	}
	h := newHTTPProxy(prepared.Bundle, nil, nil, nil, nil, nil, nil)

	cfg, err := h.mitmConnect.TLSConfig("Example.Test.:443", nil)
	if err != nil {
		t.Fatalf("TLSConfig() error = %v", err)
	}
	leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate() error = %v", err)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "example.test" {
		t.Fatalf("leaf DNSNames = %q, want [\"example.test\"]", leaf.DNSNames)
	}
	ca, err := x509.ParseCertificate(prepared.Bundle.MITMCA.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate(CA) error = %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "example.test.", Roots: roots}); err != nil {
		t.Fatalf("leaf does not verify for example.test.: %v", err)
	}
}
