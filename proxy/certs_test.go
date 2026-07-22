package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"
	"time"
)

func TestPrepareCertificatesCreatesClientMaterial(t *testing.T) {
	dir := t.TempDir()
	prepared, err := PrepareCertificates(PrepareOptions{
		Dir:         dir,
		ProxyURL:    "https://worker-proxy:17080",
		ServerHosts: []string{"worker-proxy", "127.0.0.1"},
		ClientIDs:   []string{"sandbox-1"},
	})
	if err != nil {
		t.Fatalf("PrepareCertificates() error = %v", err)
	}
	if prepared.Bundle == nil {
		t.Fatal("expected bundle")
	}
	client := prepared.Clients["sandbox-1"]
	if client.ClientCertPath == "" || client.ClientKeyPath == "" {
		t.Fatalf("expected client cert paths: %#v", client)
	}
	if _, err := os.Stat(client.MITMCAPath); err != nil {
		t.Fatalf("MITM CA missing: %v", err)
	}
	if _, err := tls.LoadX509KeyPair(client.ClientCertPath, client.ClientKeyPath); err != nil {
		t.Fatalf("client certificate not loadable: %v", err)
	}
	if got := client.EnvironmentVars["HTTPS_PROXY"]; got != "https://worker-proxy:17080" {
		t.Fatalf("HTTPS_PROXY = %q", got)
	}
}

func TestCertificateBundleSignsHost(t *testing.T) {
	prepared, err := PrepareCertificates(PrepareOptions{Dir: t.TempDir(), ClientIDs: []string{"sandbox-1"}})
	if err != nil {
		t.Fatalf("PrepareCertificates() error = %v", err)
	}
	cert, err := prepared.Bundle.SignHost("api.example.com")
	if err != nil {
		t.Fatalf("SignHost() error = %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("expected signed certificate")
	}
}

func TestPrepareCertificatesRenewsExpiringClientCertificate(t *testing.T) {
	dir := t.TempDir()
	prepared, err := PrepareCertificates(PrepareOptions{
		Dir:            dir,
		ClientIDs:      []string{"sandbox-1"},
		ClientValidity: 2 * time.Hour,
		RenewBefore:    4 * time.Hour,
	})
	if err != nil {
		t.Fatalf("PrepareCertificates() error = %v", err)
	}
	first := clientSerial(t, prepared.Clients["sandbox-1"].ClientCertPath)

	renewed, err := PrepareCertificates(PrepareOptions{
		Dir:            dir,
		ClientIDs:      []string{"sandbox-1"},
		ClientValidity: 2 * time.Hour,
		RenewBefore:    4 * time.Hour,
	})
	if err != nil {
		t.Fatalf("second PrepareCertificates() error = %v", err)
	}
	second := clientSerial(t, renewed.Clients["sandbox-1"].ClientCertPath)
	if first == second {
		t.Fatal("expected expiring client certificate to be renewed")
	}
}

func clientSerial(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("missing certificate PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert.SerialNumber.String()
}

// A server certificate that predates a rename of the proxy's DNS name is still
// in date but no longer covers the name clients dial, so preparation must
// reissue it rather than reuse it.
func TestPrepareCertificatesReissuesServerCertWhenHostsChange(t *testing.T) {
	dir := t.TempDir()

	if _, err := PrepareCertificates(PrepareOptions{
		Dir:         dir,
		ProxyURL:    "https://old-name:17080",
		ServerHosts: []string{"old-name", "127.0.0.1", "localhost"},
	}); err != nil {
		t.Fatalf("first PrepareCertificates() error = %v", err)
	}

	prepared, err := PrepareCertificates(PrepareOptions{
		Dir:         dir,
		ProxyURL:    "https://new-name:17080",
		ServerHosts: []string{"new-name", "127.0.0.1", "localhost"},
	})
	if err != nil {
		t.Fatalf("second PrepareCertificates() error = %v", err)
	}

	leaf, err := parseLeaf(prepared.Bundle.ServerCert)
	if err != nil {
		t.Fatalf("parseLeaf() error = %v", err)
	}
	if err := leaf.VerifyHostname("new-name"); err != nil {
		t.Fatalf("server certificate not reissued for the new host: %v", err)
	}
	// The CAs are the host's trust root and must survive the reissue, or every
	// already-distributed client certificate stops verifying.
	if err := leaf.VerifyHostname("localhost"); err != nil {
		t.Fatalf("server certificate lost a retained host: %v", err)
	}
}
