package credentials

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/agentcreds"
	"github.com/discobox-ai/discobox/wire"
)

const poolServerName = "discobox-pool-proxy"

// issue writes a certificate and key signed by parent (self-signed when parent
// is nil) and returns the certificate.
func issue(t *testing.T, dir, name string, template *x509.Certificate, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if parent == nil {
		parent, parentKey = template, key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// A sandbox in a host-VM pool reaches the pool's credentials endpoint through a
// socket rather than a network (ADR 0144 §4). The relay must dial the unix URL
// bridge.json names and still present the sandbox's client certificate, since
// that certificate is the only identity the pool serves the request for.
func TestRelayDialsUnixURLWithMTLS(t *testing.T) {
	// Short on purpose: a socket path has a length limit t.TempDir can exceed.
	dir, err := os.MkdirTemp("", "cr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	notAfter := time.Now().Add(time.Hour)
	ca, caKey := issue(t, dir, "mtls-ca", &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "pool mTLS CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}, nil, nil)
	issue(t, dir, "server", &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: poolServerName},
		DNSNames:     []string{poolServerName},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     notAfter,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}, ca, caKey)
	issue(t, dir, "client", &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "sandbox-1"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     notAfter,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}, ca, caKey)

	serverCert, err := tls.LoadX509KeyPair(filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(ca)
	socket := filepath.Join(dir, "pool.sock")
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	// The pool answers for whoever the client certificate names, so the
	// credential it lists here is the identity it saw.
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+agentcreds.PathCredentials, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(agentcreds.ListResponse{Credentials: []agentcreds.Credential{{
			Name:   r.TLS.PeerCertificates[0].Subject.CommonName,
			EnvVar: "SEEN_AS",
		}}})
	})
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{serverCert},
			ClientCAs:    clientCAs,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			MinVersion:   tls.VersionTLS12,
		},
	}
	go func() { _ = server.ServeTLS(listener, "", "") }()
	t.Cleanup(func() { _ = server.Close() })

	config, err := json.Marshal(bridgeConfig{
		CredentialsURL: wire.UnixURL(socket),
		ServerName:     poolServerName,
		MTLSCAPath:     filepath.Join(dir, "mtls-ca.crt"),
		ClientCertPath: filepath.Join(dir, "client.crt"),
		ClientKeyPath:  filepath.Join(dir, "client.key"),
	})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "bridge.json")
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}

	relay, err := New(configPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	credentials, err := relay.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(credentials) != 1 || credentials[0].Name != "sandbox-1" {
		t.Fatalf("pool saw %+v, want one credential naming sandbox-1", credentials)
	}
}
