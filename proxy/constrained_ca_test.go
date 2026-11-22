package proxy

import (
	"crypto/x509"
	"path/filepath"
	"testing"
	"time"
)

// The MITM CA restated for some hosts vouches for a leaf the proxy mints for
// one of them, and for no other host's, of either kind of name.
func TestConstrainedMITMCAVouchesForItsHostsAlone(t *testing.T) {
	prepared, err := PrepareCertificates(PrepareOptions{Dir: filepath.Join(t.TempDir(), "certs"), ProxyURL: "https://127.0.0.1:0", ServerHosts: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	bundle := prepared.Bundle
	for _, tc := range []struct {
		hosts   []string
		trusted []string
		refused []string
	}{
		{[]string{"control.example.com"}, []string{"control.example.com"}, []string{"process.example.com", "example.com", "127.0.0.1", "10.0.0.1"}},
		{[]string{"127.0.0.1"}, []string{"127.0.0.1"}, []string{"127.0.0.2", "control.example.com"}},
	} {
		ca, err := bundle.ConstrainedMITMCA(tc.hosts)
		if err != nil {
			t.Fatal(err)
		}
		roots := x509.NewCertPool()
		roots.AddCert(ca)
		verify := func(host string) error {
			leaf, err := bundle.SignHost(host)
			if err != nil {
				t.Fatal(err)
			}
			cert, err := x509.ParseCertificate(leaf.Certificate[0])
			if err != nil {
				t.Fatal(err)
			}
			_, err = cert.Verify(x509.VerifyOptions{DNSName: host, Roots: roots, CurrentTime: time.Now()})
			return err
		}
		for _, host := range tc.trusted {
			if err := verify(host); err != nil {
				t.Errorf("restated for %v: a leaf for %s = %v, want it trusted", tc.hosts, host, err)
			}
		}
		for _, host := range tc.refused {
			if err := verify(host); err == nil {
				t.Errorf("restated for %v: a leaf for %s was trusted", tc.hosts, host)
			}
		}
	}
}
