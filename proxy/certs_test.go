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
