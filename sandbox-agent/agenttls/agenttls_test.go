package agenttls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

// authority is a CA that signs a server certificate for a sandbox, as a
// pool's mTLS CA does.
type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newAuthority(t *testing.T) *authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test mTLS CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &authority{cert: cert, key: key, pool: pool}
}

// place writes a keypair for name into dir with serial, as the pool places
// it.
func (a *authority) place(t *testing.T, dir, name string, serial int64) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(file string, block *pem.Block) {
		// A replacement lands with a new modification time even within the
		// filesystem's clock resolution.
		path := filepath.Join(dir, file)
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(time.Duration(serial) * time.Second)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	write(sandboxconfig.AgentTLSCertName, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	write(sandboxconfig.AgentTLSKeyName, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// serve serves the agent's API over TLS with keypair and returns its address.
func serve(t *testing.T, keypair *Keypair) string {
	t.Helper()
	var listenConfig net.ListenConfig
	inner, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "agent") }),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = server.Serve(tls.NewListener(inner, Config(keypair))) }()
	t.Cleanup(func() { _ = server.Close() })
	return inner.Addr().String()
}

// getTLS asks the agent at address over TLS, verifying it for serverName
// against ca, and returns the connection's state.
func getTLS(t *testing.T, ca *authority, serverName, address string) (*tls.ConnectionState, error) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: ca.pool, ServerName: serverName, MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+address+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.TLS, nil
}

// The agent serves its API over TLS with the keypair its pool placed,
// verified for the sandbox the certificate names and no other.
func TestTheAgentServesTheKeypairItsPoolPlaced(t *testing.T) {
	ca := newAuthority(t)
	dir := t.TempDir()
	ca.place(t, dir, "sbx-1.sandbox.discobox.internal", 2)
	address := serve(t, NewKeypair(dir))

	if _, err := getTLS(t, ca, "sbx-1.sandbox.discobox.internal", address); err != nil {
		t.Fatalf("TLS to the agent: %v", err)
	}
	if _, err := getTLS(t, ca, "sbx-2.sandbox.discobox.internal", address); err == nil {
		t.Fatal("a pool meaning to reach sbx-2 accepted sbx-1's agent")
	}
}

// A renewed keypair is served from the next handshake, with no restart, even
// when its files have the size and time the old ones had, as a machine's
// place step writes them.
func TestARenewedKeypairIsServedFromTheNextHandshake(t *testing.T) {
	ca := newAuthority(t)
	dir := t.TempDir()
	ca.place(t, dir, "sbx-1.sandbox.discobox.internal", 2)
	address := serve(t, NewKeypair(dir))
	serial := func() int64 {
		t.Helper()
		state, err := getTLS(t, ca, "sbx-1.sandbox.discobox.internal", address)
		if err != nil {
			t.Fatal(err)
		}
		return state.PeerCertificates[0].SerialNumber.Int64()
	}
	if got := serial(); got != 2 {
		t.Fatalf("served serial %d, want 2", got)
	}
	ca.place(t, dir, "sbx-1.sandbox.discobox.internal", 3)
	epoch := time.Unix(0, 0)
	for _, name := range []string{sandboxconfig.AgentTLSCertName, sandboxconfig.AgentTLSKeyName} {
		if err := os.Chtimes(filepath.Join(dir, name), epoch, epoch); err != nil {
			t.Fatal(err)
		}
	}
	if got := serial(); got != 3 {
		t.Fatalf("served serial %d after renewal, want 3", got)
	}
}

// An agent with no keypair placed serves no TLS: the handshake fails rather
// than serving anything else.
func TestWithoutAKeypairNoTLSIsServed(t *testing.T) {
	address := serve(t, NewKeypair(filepath.Join(t.TempDir(), "absent")))
	if _, err := getTLS(t, newAuthority(t), "sbx-1.sandbox.discobox.internal", address); err == nil {
		t.Fatal("TLS was served with no keypair")
	}
}
