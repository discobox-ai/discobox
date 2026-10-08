package bridge_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy"
	"github.com/discobox-ai/discobox/proxy/bridge"
)

// renew reissues the sandbox's client certificate in place, the way the pool
// does inside its renewal window: a window longer than any validity makes it
// due now.
func renew(t *testing.T, bundle *proxy.CertificateBundle) {
	t.Helper()
	if _, err := proxy.EnsureClientCertificate(bundle, "sandbox-1", "", "", 24*time.Hour, 100*365*24*time.Hour); err != nil {
		t.Fatalf("renew client certificate: %v", err)
	}
}

// issueClient writes a client keypair for sandbox-1, signed by the pool's mTLS
// CA, valid from notBefore to notAfter.
func issueClient(t *testing.T, bundle *proxy.CertificateBundle, material proxy.ClientMaterial, notBefore, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(bundle.MTLSCA.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "sandbox-1"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, bundle.MTLSCA.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, material.ClientCertPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeFile(t, material.ClientKeyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func dialSerial(t *testing.T, dialer *bridge.Dialer) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := dialer.Dial(ctx)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_, _, serial := readIdentity(t, conn)
	return serial
}

// A sandbox whose certificate the pool renews keeps proxying, without its
// bridge restarting: a connection already open carries on, and the next one
// presents the renewed certificate (#62, ADR 0126 §7).
func TestForwarderPresentsARenewedCertificateWithoutRestart(t *testing.T) {
	prepared := poolMaterial(t)
	url := unixPool(t, prepared.Bundle)
	material := prepared.Clients["sandbox-1"]
	forwarder, err := bridge.New(context.Background(), bridge.Config{
		WorkerProxyURL: url,
		ServerName:     poolServerName,
		MTLSCAPath:     material.MTLSCAPath,
		ClientCertPath: material.ClientCertPath,
		ClientKeyPath:  material.ClientKeyPath,
	})
	if err != nil {
		t.Fatalf("bridge.New: %v", err)
	}
	var listenConfig net.ListenConfig
	local, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = forwarder.Serve(local) }()
	t.Cleanup(func() { _ = forwarder.Close() })
	connect := func() (net.Conn, string) {
		t.Helper()
		var dialer net.Dialer
		conn, err := dialer.DialContext(context.Background(), "tcp", local.Addr().String())
		if err != nil {
			t.Fatalf("dial forwarder: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		_, identity, serial := readIdentity(t, conn)
		if identity != "sandbox-1" {
			t.Fatalf("pool saw client %q, want sandbox-1", identity)
		}
		return conn, serial
	}

	before, original := connect()
	renew(t, prepared.Bundle)
	_, renewed := connect()
	if renewed == original {
		t.Fatalf("the connection after renewal presented the original certificate %s", original)
	}
	// The connection opened before the renewal is still the one it was.
	if _, err := io.WriteString(before, "ping\n"); err != nil {
		t.Fatalf("write on the earlier connection: %v", err)
	}
	echo := make([]byte, len("ping\n"))
	if _, err := io.ReadFull(before, echo); err != nil || string(echo) != "ping\n" {
		t.Fatalf("earlier connection after renewal: %q, %v", echo, err)
	}
}

// The intake replaces the certificate and the key one after the other. A
// handshake between the two finds a pair that does not match, and presents the
// pair it last held rather than failing; the next one after the key lands
// presents the new pair.
func TestDialerKeepsItsPairWhileADeliveryIsHalfWay(t *testing.T) {
	prepared := poolMaterial(t)
	url := unixPool(t, prepared.Bundle)
	material := prepared.Clients["sandbox-1"]
	dialer, err := bridge.NewDialer(dialConfig(material, url, poolServerName))
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	original := dialSerial(t, dialer)
	oldKey := readFile(t, material.ClientKeyPath)
	renew(t, prepared.Bundle)
	newKey := readFile(t, material.ClientKeyPath)
	writeFile(t, material.ClientKeyPath, oldKey)
	if got := dialSerial(t, dialer); got != original {
		t.Fatalf("half way through a delivery the dialer presented %s, want the held %s", got, original)
	}
	writeFile(t, material.ClientKeyPath, newKey)
	if got := dialSerial(t, dialer); got == original {
		t.Fatalf("after the delivery finished the dialer still presented %s", got)
	}
}

// An expired certificate is never presented. The dialer refuses before it
// dials, and serves again once the renewal lands — whether the expired
// certificate was delivered over a good one or was what the bridge started on.
func TestDialerNeverPresentsAnExpiredCertificate(t *testing.T) {
	prepared := poolMaterial(t)
	url := unixPool(t, prepared.Bundle)
	material := prepared.Clients["sandbox-1"]
	expire := func() {
		issueClient(t, prepared.Bundle, material, time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
	}
	refused := func(dialer *bridge.Dialer) {
		t.Helper()
		conn, err := dialer.Dial(context.Background())
		if err == nil {
			_ = conn.Close()
			t.Fatal("Dial succeeded with an expired client certificate")
		}
		if !strings.Contains(err.Error(), "expired") {
			t.Fatalf("Dial error = %v, want one saying the certificate expired", err)
		}
	}

	dialer, err := bridge.NewDialer(dialConfig(material, url, poolServerName))
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	dialSerial(t, dialer)
	expire()
	refused(dialer)
	renew(t, prepared.Bundle)
	dialSerial(t, dialer)

	expire()
	started, err := bridge.NewDialer(dialConfig(material, url, poolServerName))
	if err != nil {
		t.Fatalf("NewDialer on an expired certificate: %v", err)
	}
	refused(started)
	renew(t, prepared.Bundle)
	dialSerial(t, started)
}
